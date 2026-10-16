package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/grant"
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
	net   *Membership
	// addrsOf lists known addresses of a device (set once the node exists).
	addrsOf func(device string) []string

	reqMu    sync.Mutex
	requests map[string]*approval

	mu       sync.Mutex
	fails    map[peer.ID]int
	banned   map[peer.ID]time.Time
	cachedAt time.Time
	cached   bool
}

func newPairer(home string, log *slog.Logger, m *Membership) *pairer {
	return &pairer{home: home, store: pairing.NewStore(home), log: log, net: m,
		fails: map[peer.ID]int{}, banned: map[peer.ID]time.Time{}, requests: map[string]*approval{}}
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

func (p *pairer) Accept(remote peer.ID, remoteIP string, req protocol.PairRequest, extend func(time.Duration)) (protocol.PairResponse, func()) {
	pend, ok := p.store.Consume(req.Token, time.Now())
	if !ok {
		p.fail(remote)
		return protocol.PairResponse{Err: "invalid, expired or already used invitation"}, nil
	}
	if pend.Net {
		return p.acceptNetwork(remote, remoteIP, req, pend, extend)
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
	Dir     string // network invitation: the directory under which every granted folder is created
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

// CheckJoinPath verifies, before an invitation is spent, that the folder this
// device would create does not overlap another folder or the private data
// directory (SaveConfig would refuse it afterwards, when the token is gone).
func CheckJoinPath(home string, cfg *config.Config, inv pairing.Invite, o JoinOptions) error {
	if cfg.Folder(inv.Folder) != nil || o.Path == "" {
		return nil
	}
	abs, err := NormalizePath(o.Path)
	if err != nil {
		return err
	}
	probe := *cfg
	probe.Folders = append(append([]config.Folder{}, cfg.Folders...), config.Folder{ID: inv.Folder, Path: abs})
	return probe.CheckPaths(home)
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

// ---- joining a network through an admin ------------------------------------------------------

// approval is a guest waiting for an admin's decision.
type approval struct {
	Request
	decide chan bool
}

// Request describes a device asking to join, for the admin to approve.
type Request struct {
	ID       string    `json:"id"`
	DeviceID string    `json:"device_id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip,omitempty"`
	Folders  []string  `json:"folders"`
	AsRelay  bool      `json:"as_relay,omitempty"`
	Admin    bool      `json:"admin,omitempty"`
	Since    time.Time `json:"since"`
}

const maxPendingRequests = 4

// Requests lists the devices waiting for an admin's decision.
func (p *pairer) Requests() []Request {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	out := make([]Request, 0, len(p.requests))
	for _, a := range p.requests {
		out = append(out, a.Request)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Decide answers a waiting request. It reports whether the request still existed.
func (p *pairer) Decide(id string, accept bool) bool {
	p.reqMu.Lock()
	a, ok := p.requests[id]
	p.reqMu.Unlock()
	if !ok {
		return false
	}
	select {
	case a.decide <- accept:
	default:
	}
	return true
}

// await registers a request and waits for the admin (or the time limit).
func (p *pairer) await(r Request, extend func(time.Duration)) bool {
	a := &approval{Request: r, decide: make(chan bool, 1)}
	p.reqMu.Lock()
	if len(p.requests) >= maxPendingRequests {
		p.reqMu.Unlock()
		return false
	}
	p.requests[r.ID] = a
	p.reqMu.Unlock()
	defer func() {
		p.reqMu.Lock()
		delete(p.requests, r.ID)
		p.reqMu.Unlock()
	}()
	extend(pairing.MaxApprovalWait)
	t := time.NewTimer(pairing.MaxApprovalWait)
	defer t.Stop()
	select {
	case ok := <-a.decide:
		return ok
	case <-t.C:
		return false
	}
}

func (p *pairer) acceptNetwork(remote peer.ID, remoteIP string, req protocol.PairRequest, pend pairing.Pending, extend func(time.Duration)) (protocol.PairResponse, func()) {
	refuse := func(msg string) (protocol.PairResponse, func()) { return protocol.PairResponse{Err: msg}, nil }
	guest := remote.String()
	guestName := config.Clean(req.Name)

	cfg, err := LoadConfig(p.home)
	if err != nil {
		return refuse("internal error")
	}
	if !cfg.Admin || p.net == nil {
		return refuse("this device is not a network admin")
	}
	for _, id := range pend.Folders {
		f := cfg.Folder(id)
		if f == nil {
			return refuse("a folder of the invitation no longer exists")
		}
		if f.Role == config.RoleRelay && !pend.InviteeRelay {
			return refuse("this device is a relay for a folder of the invitation; it can only introduce relays")
		}
	}
	if pend.Approve {
		r := Request{ID: grant.RandomID(), DeviceID: guest, Name: guestName, IP: remoteIP, Folders: pend.Folders,
			AsRelay: pend.InviteeRelay, Admin: pend.GuestAdmin, Since: time.Now()}
		p.log.Warn("a device asks to join the network; approve it with `p2psync requests accept`",
			"request", r.ID, "device", guest, "name", guestName)
		if !p.await(r, extend) {
			p.log.Info("join request declined or timed out", "request", r.ID)
			return refuse("the admin did not accept this request")
		}
	}

	// A copy of the configuration with the guest in it tells which grants concern it.
	plan := *cfg
	plan.Devices = slices.Clone(cfg.Devices)
	plan.Folders = make([]config.Folder, len(cfg.Folders))
	for i, f := range cfg.Folders {
		plan.Folders[i] = f
		plan.Folders[i].Devices = slices.Clone(f.Devices)
	}
	if err := AddDevice(&plan, guest, guestName, nil); err != nil {
		return refuse("internal error")
	}
	plan.Device(guest).Admin = pend.GuestAdmin
	secrets := map[string]string{}
	for _, id := range pend.Folders {
		if err := ShareFolder(&plan, id, guest, pend.InviteeRelay); err != nil {
			return refuse("internal error")
		}
		if f := cfg.Folder(id); !pend.InviteeRelay && f.Secret != "" {
			secrets[id] = f.Secret // only a full guest ever receives a folder secret
		}
	}
	if _, err := p.net.IssueFromConfig(&plan); err != nil {
		p.log.Error("cannot issue the grant", "err", err)
		return refuse("internal error")
	}
	resp := protocol.PairResponse{OK: true, Name: cfg.DeviceName, Folder: pend.Folders[0], Network: true,
		Grants: p.net.ForPeer(&plan, guest), Secrets: secrets, Hints: p.hintsFor(&plan, guest)}

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
			if err = AddDevice(cfg, guest, guestName, addrs); err == nil {
				cfg.Device(guest).Admin = pend.GuestAdmin
				for _, id := range pend.Folders {
					if err = ShareFolder(cfg, id, guest, pend.InviteeRelay); err != nil {
						break
					}
				}
			}
			if err == nil {
				err = SaveConfig(p.home, cfg) // the daemon notices and reloads
			}
		}
		if err != nil {
			p.log.Error("the device was introduced but could not be recorded; run the invitation again", "err", err)
		}
	}
	return resp, commit
}

// hintsFor gives, for each device the guest will hear about, the addresses this
// device knows for it.
func (p *pairer) hintsFor(cfg *config.Config, guest string) map[string][]string {
	hints := map[string][]string{}
	for _, sg := range p.net.ForPeer(cfg, guest) {
		g, err := grant.Verify(sg)
		if err != nil || g.Subject == guest || g.Subject == p.net.id.ID || len(hints) >= protocol.MaxHintedIDs {
			continue
		}
		var addrs []string
		if d := cfg.Device(g.Subject); d != nil {
			addrs = append(addrs, d.Addresses...)
		}
		if p.addrsOf != nil {
			addrs = append(addrs, p.addrsOf(g.Subject)...)
		}
		if addrs = uniqueFirst(addrs, protocol.MaxHintAddrs); len(addrs) > 0 {
			hints[g.Subject] = addrs
		}
	}
	return hints
}

func uniqueFirst(in []string, n int) []string {
	var out []string
	for _, a := range in {
		if a != "" && len(a) <= 200 && !slices.Contains(out, a) {
			out = append(out, a)
		}
		if len(out) >= n {
			break
		}
	}
	return out
}

// NetworkInviteOptions chooses what a network invitation offers.
type NetworkInviteOptions struct {
	Folders []string // folder IDs to share with the guest (empty: every folder)
	AsRelay bool     // the guest is a temporary relay (a phone), not a full copy
	Admin   bool     // the guest becomes an admin
	Approve bool     // the admin confirms the request by hand
	TTL     time.Duration
}

// CreateNetworkInvite issues a single-use invitation that lets a new device join
// this admin's network: it will be introduced, with the folders chosen here, to
// the devices that share them.
func CreateNetworkInvite(home string, cfg *config.Config, selfID string, addrs []string, o NetworkInviteOptions) (pairing.Invite, error) {
	if !cfg.Admin {
		return pairing.Invite{}, errors.New("this device is not a network admin: run `p2psync admin enable` first")
	}
	folders := slices.Clone(o.Folders)
	if len(folders) == 0 {
		for _, f := range cfg.Folders {
			folders = append(folders, f.ID)
		}
	}
	if len(folders) == 0 {
		return pairing.Invite{}, errors.New("there is no folder to share yet")
	}
	if len(folders) > 64 {
		return pairing.Invite{}, errors.New("too many folders in one invitation")
	}
	sort.Strings(folders)
	folders = slices.Compact(folders)
	for _, id := range folders {
		f := cfg.Folder(id)
		if f == nil {
			return pairing.Invite{}, fmt.Errorf("unknown folder %q", id)
		}
		if f.Role == config.RoleRelay && !o.AsRelay {
			return pairing.Invite{}, fmt.Errorf("this device is only a relay for %q: it can only introduce other relays "+
				"(invite a full device from a device that holds the folder)", id)
		}
	}
	if o.AsRelay && o.Admin {
		return pairing.Invite{}, errors.New("a relay cannot be an admin: it would be able to introduce devices that obtain the folder secret")
	}
	sel := SelectAddrs(addrs, selfID)
	if len(sel) == 0 {
		return pairing.Invite{}, errors.New("this device has no address to share yet (is the daemon running and connected to a network?)")
	}
	tok, err := pairing.NewToken()
	if err != nil {
		return pairing.Invite{}, err
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = pairing.DefaultTTL
	}
	ttl = min(ttl, 24*time.Hour)
	inv := pairing.Invite{V: 2, ID: selfID, Name: cfg.DeviceName, Addrs: sel, Token: tok, Folder: folders[0],
		InviteeRelay: o.AsRelay, Exp: time.Now().Add(ttl).Unix(), Net: true, Folders: folders, Admin: o.Admin, Approve: o.Approve}
	err = pairing.NewStore(home).Add(tok, pairing.Pending{Folder: folders[0], InviteeRelay: o.AsRelay, Exp: inv.Exp,
		Net: true, Folders: folders, GuestAdmin: o.Admin, Approve: o.Approve})
	return inv, err
}

// JoinNetwork applies an admin's answer: the admin is trusted from now on, its
// grants (checked one by one) tell which folders and devices this device takes
// part in, and the folders are created under o.Dir.
func JoinNetwork(m *Membership, cfg *config.Config, inv pairing.Invite, resp protocol.PairResponse, o JoinOptions) error {
	if !inv.Net || !resp.Network {
		return errors.New("the other device did not answer a network invitation")
	}
	if resp.Secret != "" {
		return errors.New("unexpected folder secret in a network answer; refusing")
	}
	if o.Dir != "" {
		dir, err := NormalizePath(o.Dir)
		if err != nil {
			return err
		}
		cfg.NetworkDir = dir
	}
	if cfg.NetworkDir == "" {
		return errors.New("choose where the folders go with --dir <directory>")
	}
	if len(resp.Grants) > protocol.MaxGrants {
		return errors.New("too many grants")
	}
	name := config.Clean(resp.Name)
	if name == "" {
		name = inv.Name
	}
	if err := AddDevice(cfg, inv.ID, name, inv.Addrs); err != nil {
		return err
	}
	cfg.Device(inv.ID).Admin = true // the owner scanned this code: this device is trusted as an admin

	pins := pinned(cfg, m.id.ID)
	for pass := 0; pass < 4; pass++ {
		progress := false
		for _, sg := range resp.Grants {
			if _, ch, err := m.set.Offer(sg, pins, time.Now()); err == nil && ch {
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	mine, ok := m.set.Get(m.id.ID)
	if !ok || mine.Revoked || len(mine.Folders) == 0 {
		return errors.New("the admin did not send a valid grant for this device")
	}
	// A folder this device already has: the admin joins it.
	for _, sh := range mine.Folders {
		f := cfg.Folder(sh.ID)
		if f == nil {
			continue
		}
		if (f.Role == config.RoleRelay) != sh.Relay {
			return fmt.Errorf("folder %q already exists here with another role than the admin granted", sh.ID)
		}
		adminRelay := false
		if self, ok := m.set.GetSelf(inv.ID); ok {
			if s, has := self.Has(sh.ID); has {
				adminRelay = s.Relay
			}
		}
		if err := ShareFolder(cfg, sh.ID, inv.ID, adminRelay); err != nil {
			return err
		}
	}
	m.Apply(cfg)

	for id, sec := range resp.Secrets {
		f := cfg.Folder(id)
		if f == nil || f.Role != config.RoleFull || !f.AwaitingSecret {
			continue
		}
		if _, err := seal.ParseSecret(sec); err != nil {
			continue
		}
		f.Secret, f.AwaitingSecret = sec, false
	}
	// Where to find the devices that were just introduced.
	for id, addrs := range resp.Hints {
		if d := cfg.Device(id); d != nil && d.Managed && len(d.Addresses) == 0 {
			d.Addresses = uniqueFirst(addrs, protocol.MaxHintAddrs)
		}
	}
	return m.set.Save()
}

// PreflightNetwork checks, before a network invitation is spent, that this device
// can act on it: folders it already has must have the role the admin will grant.
func PreflightNetwork(cfg *config.Config, inv pairing.Invite) error {
	for _, id := range inv.Folders {
		if f := cfg.Folder(id); f != nil && (f.Role == config.RoleRelay) != inv.InviteeRelay {
			return fmt.Errorf("folder %q already exists here with another role than the invitation gives", id)
		}
	}
	return nil
}
