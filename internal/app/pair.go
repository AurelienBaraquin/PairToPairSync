package app

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// ---- inviter side -------------------------------------------------------------------

// pairer answers invitations on behalf of the running daemon.
type pairer struct {
	home  string
	store *pairing.Store

	mu    sync.Mutex
	fails int
}

func newPairer(home string) *pairer { return &pairer{home: home, store: pairing.NewStore(home)} }

func (p *pairer) Active() bool { return p.store.Active(time.Now()) }

const maxBadTokens = 5

func (p *pairer) Accept(remote peer.ID, remoteIP string, req protocol.PairRequest) (protocol.PairResponse, func()) {
	p.mu.Lock()
	pend, ok := p.store.Consume(req.Token, time.Now())
	if !ok {
		p.fails++
		if p.fails >= maxBadTokens { // someone is guessing: cancel every invitation
			p.store.Purge()
			p.fails = 0
		}
		p.mu.Unlock()
		return protocol.PairResponse{Err: "invalid, expired or already used invitation"}, nil
	}
	p.fails = 0
	p.mu.Unlock()

	cfg, err := LoadConfig(p.home)
	if err != nil {
		return protocol.PairResponse{Err: "internal error"}, nil
	}
	f := cfg.Folder(pend.Folder)
	if f == nil {
		return protocol.PairResponse{Err: "the folder no longer exists"}, nil
	}
	inviterRelay := f.Role == config.RoleRelay
	guestRelay := pend.InviteeRelay && !inviterRelay
	resp := protocol.PairResponse{OK: true, Name: cfg.DeviceName, Folder: f.ID, Label: f.Label, InviterRelay: inviterRelay}
	if !inviterRelay && !guestRelay {
		resp.Secret = f.Secret // only a full guest ever receives the folder secret
	}

	guestName := cleanName(req.Name)
	var addr string
	if remoteIP != "" && req.Port > 0 && req.Port < 65536 {
		addr = net.JoinHostPort(remoteIP, strconv.Itoa(req.Port))
	}
	commit := func() {
		cfg, err := LoadConfig(p.home)
		if err != nil {
			return
		}
		var addrs []string
		if addr != "" {
			addrs = []string{addr}
		}
		if AddDevice(cfg, remote.String(), guestName, addrs) != nil ||
			ShareFolder(cfg, pend.Folder, remote.String(), guestRelay) != nil {
			return
		}
		_ = SaveConfig(p.home, cfg) // the daemon notices and reloads
	}
	return resp, commit
}

func cleanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) && b.Len() < 64 {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// CreateInvite issues a single-use invitation for folderID. addrs are the
// inviter's own multiaddrs as the running daemon reports them.
func CreateInvite(home string, cfg *config.Config, selfID string, addrs []string, folderID string, asRelay bool, ttl time.Duration) (pairing.Invite, error) {
	f := cfg.Folder(folderID)
	if f == nil {
		return pairing.Invite{}, fmt.Errorf("unknown folder %q", folderID)
	}
	inviterRelay := f.Role == config.RoleRelay
	if inviterRelay && asRelay {
		return pairing.Invite{}, errors.New("this device is a relay: it can only invite full devices")
	}
	sel := SelectAddrs(addrs, selfID)
	if len(sel) == 0 {
		return pairing.Invite{}, errors.New("this device has no address to share yet (is the daemon running and connected to a network?)")
	}
	tok, err := pairing.NewToken()
	if err != nil {
		return pairing.Invite{}, err
	}
	if ttl <= 0 {
		ttl = pairing.DefaultTTL
	}
	exp := time.Now().Add(ttl)
	inv := pairing.Invite{V: 1, ID: selfID, Name: cfg.DeviceName, Addrs: sel, Token: tok, Folder: f.ID, Label: f.Label,
		InviterRelay: inviterRelay, InviteeRelay: asRelay, Exp: exp.Unix()}
	if err := pairing.NewStore(home).Add(tok, pairing.Pending{Folder: f.ID, InviteeRelay: asRelay, Exp: inv.Exp}); err != nil {
		return pairing.Invite{}, err
	}
	return inv, nil
}

// SelectAddrs keeps the few addresses most useful to a guest, so the QR code
// stays small: a public one, a local-network one, a relayed one, and only if
// nothing else exists, loopback.
func SelectAddrs(all []string, selfID string) []string {
	var public, lan, circuit, other, loop []string
	for _, a := range all {
		a = strings.TrimSuffix(a, "/p2p/"+selfID)
		parts := strings.Split(a, "/")
		switch {
		case strings.Contains(a, "/p2p-circuit"):
			circuit = append(circuit, a)
		case len(parts) < 3 || (parts[1] != "ip4" && parts[1] != "ip6"):
			continue
		case !strings.Contains(a, "/quic-v1"):
			other = append(other, a)
		default:
			ip := net.ParseIP(parts[2])
			switch {
			case ip == nil || ip.IsLinkLocalUnicast():
			case ip.IsLoopback():
				loop = append(loop, a)
			case ip.IsPrivate():
				lan = append(lan, a)
			default:
				public = append(public, a)
			}
		}
	}
	var out []string
	for _, group := range [][]string{public, lan, circuit} {
		if len(group) > 0 {
			out = append(out, group[0])
		}
	}
	if len(out) == 0 && len(other) > 0 {
		out = append(out, other[0])
	}
	if len(out) == 0 && len(loop) > 0 {
		out = append(out, loop[0])
	}
	return out
}

// ---- guest side ---------------------------------------------------------------------

// JoinOptions tells Join where to put the folder on this device.
type JoinOptions struct {
	Path    string // directory of the synchronised folder (full) or of the temporary store (relay)
	QuotaMB int64  // relay only
}

// PreflightJoin checks, before the invitation is spent, that this device can
// act on it.
func PreflightJoin(cfg *config.Config, inv pairing.Invite, o JoinOptions) error {
	f := cfg.Folder(inv.Folder)
	switch {
	case inv.InviterRelay && f == nil:
		return fmt.Errorf("this invitation is from a relay for folder %q, which does not exist on this device yet: "+
			"create it first (p2psync folder add ... --id %s --secret <secret>)", inv.Folder, inv.Folder)
	case inv.InviterRelay && f.Role != config.RoleFull:
		return fmt.Errorf("folder %q on this device is not a full copy", inv.Folder)
	case !inv.InviterRelay && f == nil && o.Path == "":
		return errors.New("choose where to put the folder with --path <directory>")
	case f != nil && inv.InviteeRelay && f.Role != config.RoleRelay:
		return fmt.Errorf("folder %q already exists here as a full copy; it cannot also be a relay", inv.Folder)
	case f != nil && !inv.InviteeRelay && !inv.InviterRelay && f.Role != config.RoleFull:
		return fmt.Errorf("folder %q already exists here as a relay", inv.Folder)
	}
	return nil
}

// Join applies the inviter's answer to the local configuration.
func Join(cfg *config.Config, inv pairing.Invite, resp protocol.PairResponse, o JoinOptions) error {
	name := resp.Name
	if name == "" {
		name = inv.Name
	}
	var addrs []string
	for _, a := range inv.Addrs {
		addrs = append(addrs, a)
	}
	if err := AddDevice(cfg, inv.ID, name, addrs); err != nil {
		return err
	}
	f := cfg.Folder(inv.Folder)
	if f == nil {
		role, secret := config.RoleFull, resp.Secret
		if inv.InviteeRelay {
			role, secret = config.RoleRelay, ""
		}
		if _, err := AddFolder(cfg, inv.Folder, o.Path, role, o.QuotaMB, 0, secret); err != nil {
			return err
		}
		cfg.Folder(inv.Folder).Label = inv.Label
	} else if resp.Secret != "" && f.Secret != "" && f.Secret != resp.Secret {
		return fmt.Errorf("folder %q already has a different secret on this device; refusing to mix two folders", inv.Folder)
	} else if resp.Secret != "" && f.Secret == "" && f.Role == config.RoleFull {
		f.Secret = resp.Secret
	}
	return ShareFolder(cfg, inv.Folder, inv.ID, inv.InviterRelay)
}

// GuestRequest builds what the guest sends with the token.
func GuestRequest(cfg *config.Config) protocol.PairRequest {
	return protocol.PairRequest{Name: cfg.DeviceName, Port: cfg.ListenPort}
}

var _ = identity.Format
