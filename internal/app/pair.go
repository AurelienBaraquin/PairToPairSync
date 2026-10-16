package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
	"github.com/aurelienbaraquin/pairtopairsync/internal/seal"
)

// ---- inviter side -------------------------------------------------------------------

// cfgMu serialises read-modify-write cycles of config.json made inside this process.
var cfgMu sync.Mutex

// WithConfigLock runs fn while holding the lock that protects config.json from
// concurrent read-modify-write cycles (pairing commits, UI edits, joins).
func WithConfigLock(fn func()) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	fn()
}

const (
	maxBadTokens = 3                // failures before a device is shut out
	banDuration  = 10 * time.Minute // ... for this long
)

// pairer answers invitations on behalf of the running daemon.
type pairer struct {
	home  string
	store *pairing.Store
	log   *slog.Logger

	mu       sync.Mutex
	fails    map[peer.ID]int
	banned   map[peer.ID]time.Time
	cachedAt time.Time
	cached   bool
}

func newPairer(home string, log *slog.Logger) *pairer {
	return &pairer{home: home, store: pairing.NewStore(home), log: log,
		fails: map[peer.ID]int{}, banned: map[peer.ID]time.Time{}}
}

// Active is consulted on every inbound handshake from a stranger. A positive
// answer is cached for a second (reading and parsing invitation files is the
// costly part, and a scanner could otherwise force it); a negative one never is,
// so a freshly created invitation is honoured immediately and an empty directory
// costs almost nothing to re-check.
func (p *pairer) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.cached && now.Sub(p.cachedAt) < time.Second {
		return true
	}
	p.cached, p.cachedAt = p.store.Active(now), now
	return p.cached
}

func (p *pairer) Banned(id peer.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.banned[id]
	return ok && time.Now().Before(until)
}

// fail records a bad token. Failures are counted per device, never globally: a
// stranger must not be able to cancel everybody's invitations just by failing.
func (p *pairer) fail(id peer.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fails) > 1000 || len(p.banned) > 1000 { // bound memory against identity floods
		p.fails, p.banned = map[peer.ID]int{}, map[peer.ID]time.Time{}
	}
	if p.fails[id]++; p.fails[id] >= maxBadTokens {
		p.banned[id] = time.Now().Add(banDuration)
		delete(p.fails, id)
	}
}

func (p *pairer) Accept(remote peer.ID, remoteIP string, req protocol.PairRequest) (protocol.PairResponse, func()) {
	pend, ok := p.store.Consume(req.Token, time.Now())
	if !ok {
		p.fail(remote)
		return protocol.PairResponse{Err: "invalid, expired or already used invitation"}, nil
	}

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

	guestName := config.Clean(req.Name)
	var addr string
	if remoteIP != "" && req.Port > 0 && req.Port < 65536 {
		addr = net.JoinHostPort(remoteIP, strconv.Itoa(req.Port))
	}
	commit := func() {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		cfg, err := LoadConfig(p.home)
		if err == nil {
			var addrs []string
			if addr != "" {
				addrs = []string{addr}
			}
			if err = AddDevice(cfg, remote.String(), guestName, addrs); err == nil {
				err = ShareFolder(cfg, pend.Folder, remote.String(), guestRelay)
			}
			if err == nil {
				err = SaveConfig(p.home, cfg) // the daemon notices and reloads
			}
		}
		if err != nil {
			p.log.Error("pairing succeeded but the device could not be recorded; run the invitation again", "err", err)
		}
	}
	return resp, commit
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
	ttl = min(ttl, 24*time.Hour)
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

// Join applies the inviter's answer to the local configuration. The answer is
// checked against the invitation first: whoever made the QR code is not trusted.
func Join(cfg *config.Config, inv pairing.Invite, resp protocol.PairResponse, o JoinOptions) error {
	if resp.Folder != inv.Folder {
		return errors.New("the other device answered about a different folder than the one it invited us to")
	}
	switch {
	case inv.InviteeRelay && resp.Secret != "", inv.InviterRelay && resp.Secret != "":
		return errors.New("a relay must never be given the folder secret; refusing")
	case !inv.InviteeRelay && !inv.InviterRelay && resp.Secret == "":
		return errors.New("the other device did not provide the folder secret")
	}
	if resp.Secret != "" {
		if _, err := seal.ParseSecret(resp.Secret); err != nil {
			return err
		}
	}
	name := config.Clean(resp.Name)
	if name == "" {
		name = inv.Name
	}
	f := cfg.Folder(inv.Folder)
	// A full device that already has this folder must receive the very same
	// secret: that is what proves the inviter really belongs to it.
	if f != nil && f.Role == config.RoleFull && !inv.InviterRelay && f.Secret != "" && f.Secret != resp.Secret {
		return fmt.Errorf("folder %q already has a different secret on this device; refusing to mix two folders", inv.Folder)
	}
	if err := AddDevice(cfg, inv.ID, name, inv.Addrs); err != nil {
		return err
	}
	if f == nil {
		role, secret := config.RoleFull, resp.Secret
		if inv.InviteeRelay {
			role, secret = config.RoleRelay, ""
		}
		if _, err := AddFolder(cfg, inv.Folder, o.Path, role, o.QuotaMB, 0, secret); err != nil {
			return err
		}
		cfg.Folder(inv.Folder).Label = config.Clean(inv.Label)
	} else if f.Role == config.RoleFull && f.Secret == "" && resp.Secret != "" {
		f.Secret = resp.Secret
	}
	return ShareFolder(cfg, inv.Folder, inv.ID, inv.InviterRelay)
}

// GuestRequest builds what the guest sends with the token.
func GuestRequest(cfg *config.Config) protocol.PairRequest {
	return protocol.PairRequest{Name: cfg.DeviceName, Port: cfg.ListenPort}
}

var _ = identity.Format
