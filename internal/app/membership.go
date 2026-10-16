package app

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/grant"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
)

// GrantsFile holds the signed grants a device has received or issued.
const GrantsFile = "grants.json"

// Membership connects the signed introductions the devices of the network make of each
// other to this device's configuration.
//
// Two directions:
//   - receiving: an introduction from a trusted device says that device S takes part in
//     folders F. The matching entries are added to config.json, marked "managed" so
//     that a later introduction (a revocation, a change) can take them out again.
//     Entries the owner created by hand or by pairing are never touched.
//   - issuing: a full device describes, in signed introductions, the devices it was
//     paired with directly, so that the devices it shares folders with learn about them
//     (and they about those) without the owner pairing everything with everything.
//
// What an introduction can change is limited on purpose:
//   - the issuer must be trusted: a full device this one was paired with, or a full
//     device that such a device introduced as an introducer;
//   - only folders this device has are affected, and only if the issuer is a member of
//     them (an introduction never creates access to something the issuer cannot give);
//   - a full copy is only honoured when the issuer is itself a full member of the
//     folder: a relay is blind (it never has the folder secret), and a blind device
//     that could introduce full copies would defeat that.
type Membership struct {
	home string
	id   *identity.Identity
	log  *slog.Logger
	set  *grant.Set
}

func LoadMembership(home string, id *identity.Identity, log *slog.Logger) *Membership {
	return &Membership{home: home, id: id, log: log, set: grant.Load(filepath.Join(home, GrantsFile))}
}

// Set exposes the grants (for tests and the status page).
func (m *Membership) Set() *grant.Set { return m.set }

// pinned lists the devices trusted to introduce others without anyone else vouching for
// them: this device itself, and the full devices its owner paired it with (or set up by
// hand). A relay is never one of them.
func pinned(cfg *config.Config, self string) map[string]bool {
	p := map[string]bool{self: true}
	for _, d := range cfg.Devices {
		if !d.Managed && established(cfg, d.ID) {
			p[d.ID] = true
		}
	}
	return p
}

// ---- receiving ------------------------------------------------------------------------

// Receive takes grants offered by a connected device, keeps the valid ones and
// brings the configuration in line. It reports whether config.json changed (the
// daemon then restarts on its own). Hints about where to find devices are only
// written to the configuration when they come from a trusted device.
func (m *Membership) Receive(from string, list []grant.Signed, hints map[string][]string) (changed bool) {
	WithConfigLock(func() {
		cfg, err := LoadConfig(m.home)
		if err != nil {
			m.log.Warn("grants ignored: configuration unusable", "err", err)
			return
		}
		pins := pinned(cfg, m.id.ID)
		accepted := 0
		// Grants may arrive in any order (a promotion after what it vouches for): take
		// several passes until nothing more is accepted.
		for pass := 0; pass < 4; pass++ {
			progress := false
			for _, sg := range list {
				if _, ch, err := m.set.Offer(sg, pins, time.Now()); err == nil && ch {
					progress = true
					accepted++
				}
			}
			if !progress {
				break
			}
		}
		if accepted == 0 {
			return
		}
		if err := m.set.Save(); err != nil {
			m.log.Error("saving grants failed", "err", err)
		}
		m.log.Info("network update", "from", from[:min(len(from), 12)], "grants", accepted)
		if !m.set.Trusted(pins)[from] {
			hints = nil
		}
		changed = m.reconcileLocked(cfg, hints)
	})
	return changed
}

// Reconcile applies every stored grant to the configuration (used after joining).
func (m *Membership) Reconcile() (changed bool) {
	WithConfigLock(func() {
		cfg, err := LoadConfig(m.home)
		if err != nil {
			return
		}
		changed = m.reconcileLocked(cfg, nil)
	})
	return changed
}

func (m *Membership) reconcileLocked(cfg *config.Config, hints map[string][]string) bool {
	changed := m.Apply(cfg)
	for id, addrs := range hints { // where to find the devices just introduced
		if d := cfg.Device(id); d != nil && d.Managed && len(d.Addresses) == 0 {
			if clean := node.CleanHints(addrs); len(clean) > 0 {
				d.Addresses, changed = clean, true
			}
		}
	}
	if !changed {
		return false
	}
	if err := SaveConfig(m.home, cfg); err != nil {
		m.log.Error("could not apply the network update", "err", err)
		return false
	}
	return true
}

// Apply brings cfg in line with what the trusted devices introduced and reports whether it
// changed. Each device is handled on its own copy of the configuration and kept only if
// the result is valid, so one grant that cannot be applied never blocks the others.
//
// What a device is granted is the union of what the trusted devices say about it. A
// device with no grant at all is left alone: losing grants.json never removes anything.
func (m *Membership) Apply(cfg *config.Config) bool {
	self := m.id.ID
	trusted := m.set.Trusted(pinned(cfg, self))
	bySubject := map[string][]grant.Grant{}
	for _, e := range m.set.All() {
		g := e.Grant
		if !trusted[g.Issuer] || g.Issuer == self { // our own grants describe our configuration already
			continue
		}
		if seq, ok := cfg.Dismissed[g.Subject]; ok && g.Seq <= seq {
			continue // the owner removed this device after that grant
		}
		bySubject[g.Subject] = append(bySubject[g.Subject], g)
	}
	subjects := make([]string, 0, len(bySubject))
	for sub := range bySubject {
		subjects = append(subjects, sub)
	}
	sort.Slice(subjects, func(i, j int) bool { // this device first: it creates the folders
		if (subjects[i] == self) != (subjects[j] == self) {
			return subjects[i] == self
		}
		return subjects[i] < subjects[j]
	})
	changed := false
	// A few passes: a device vouched for by another is only honoured once that one's own
	// membership of the folder is recorded.
	for pass := 0; pass < 4; pass++ {
		progress := false
		for _, sub := range subjects {
			trial, err := cloneConfig(cfg)
			if err != nil {
				return changed
			}
			var ch bool
			if sub == self {
				ch = m.applySelf(trial, bySubject[sub])
			} else {
				ch = applyOthers(trial, sub, bySubject[sub], self)
			}
			if !ch {
				continue
			}
			if err := trial.Validate(); err == nil {
				err = trial.CheckPaths(m.home)
				if err == nil {
					*cfg, progress = *trial, true
					continue
				}
				m.log.Warn("a grant could not be applied", "device", sub[:min(len(sub), 12)], "err", err)
			} else {
				m.log.Warn("a grant could not be applied", "device", sub[:min(len(sub), 12)], "err", err)
			}
		}
		changed = changed || progress
		if !progress {
			break
		}
	}
	return changed
}

func cloneConfig(c *config.Config) (*config.Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	out := &config.Config{}
	return out, json.Unmarshal(b, out)
}

// established reports whether the owner of this configuration has reason to treat the
// device as a holder of full copies: a device it paired with as such, or one that is a
// full member of a folder it set up by hand. A device's own claim never counts, and
// neither does an introducer's: a relay could otherwise pass for a full copy and be handed
// the folder secret.
func established(cfg *config.Config, id string) bool {
	if d := cfg.Device(id); d != nil && d.Full {
		return true
	}
	for _, f := range cfg.Folders {
		for _, d := range f.Devices {
			if d.ID == id && !d.Relay && !d.Managed {
				return true
			}
		}
	}
	return false
}

// roleIn says whether device id takes part in folder f of this configuration, and as a
// relay or not.
func roleIn(f *config.Folder, self, id string) (relay, ok bool) {
	if id == self {
		return f.Role == config.RoleRelay, true
	}
	for _, d := range f.Devices {
		if d.ID == id {
			return d.Relay, true
		}
	}
	return false, false
}

// applyOthers records what the introductions say about another device: the shares the
// issuers are entitled to give, and nothing else.
func applyOthers(cfg *config.Config, subject string, gs []grant.Grant, self string) bool {
	desired := map[string]bool{} // folder -> the subject is a relay there
	var newest grant.Grant
	for _, g := range gs {
		if newest.Seq == 0 || g.Seq > newest.Seq {
			newest = g
		}
		if g.Revoked {
			continue
		}
		for _, sh := range g.Folders {
			f := cfg.Folder(sh.ID)
			if f == nil || (f.Role == config.RoleRelay && sh.Relay) { // relays do not talk to relays
				continue
			}
			// The issuer must be a member of the folder here, and a full copy can only be
			// introduced by a full member: a blind relay must not be able to make a device
			// that then obtains the secret.
			issuerRelay, member := roleIn(f, self, g.Issuer)
			if !member || (!sh.Relay && issuerRelay) {
				continue
			}
			if cur, seen := desired[sh.ID]; !seen || (cur && !sh.Relay) {
				desired[sh.ID] = sh.Relay // a full copy beats a relay when introducers disagree
			}
		}
	}
	changed := false
	for _, f := range sortedKeys(desired) {
		fo := cfg.Folder(f)
		if ensureDevice(cfg, subject, newest.Name) {
			changed = true
		}
		relay := desired[f]
		i := slices.IndexFunc(fo.Devices, func(d config.FolderDevice) bool { return d.ID == subject })
		switch {
		case i < 0:
			fo.Devices = append(fo.Devices, config.FolderDevice{ID: subject, Relay: relay, Managed: true})
			changed = true
		case fo.Devices[i].Managed && fo.Devices[i].Relay != relay:
			fo.Devices[i].Relay = relay
			changed = true
		}
	}
	keep := map[string]bool{}
	for f := range desired {
		keep[f] = true
	}
	// Silence is not removal: a managed share is withdrawn from a folder only when a
	// device entitled to speak about that folder (a member of it) said something about
	// this one and left the folder out. A grant by someone who is not (yet) a member says
	// nothing about the folder.
	for i := range cfg.Folders {
		f := &cfg.Folders[i]
		if keep[f.ID] {
			continue
		}
		heard := false
		for _, g := range gs {
			if _, member := roleIn(f, self, g.Issuer); member {
				heard = true
				break
			}
		}
		if !heard {
			keep[f.ID] = true
		}
	}
	return withdraw(cfg, subject, keep) || changed
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureDevice makes sure the device is in the configuration; it reports a change.
func ensureDevice(cfg *config.Config, id, name string) bool {
	if d := cfg.Device(id); d != nil {
		if d.Managed && name != "" && d.Name != name {
			d.Name = name
			return true
		}
		return false
	}
	cfg.Devices = append(cfg.Devices, config.Device{ID: id, Name: name, Managed: true})
	return true
}

// withdraw removes the managed shares of a device from the folders not in keep, and
// the device itself once nothing refers to it. Hand-made entries stay.
func withdraw(cfg *config.Config, subject string, keep map[string]bool) bool {
	changed := false
	for i := range cfg.Folders {
		f := &cfg.Folders[i]
		if keep[f.ID] {
			continue
		}
		n := len(f.Devices)
		f.Devices = slices.DeleteFunc(f.Devices, func(d config.FolderDevice) bool { return d.ID == subject && d.Managed })
		changed = changed || len(f.Devices) != n
	}
	if d := cfg.Device(subject); d != nil && d.Managed {
		still := false
		for _, f := range cfg.Folders {
			still = still || f.SharesWith(subject)
		}
		if !still {
			cfg.Devices = slices.DeleteFunc(cfg.Devices, func(d config.Device) bool { return d.ID == subject })
			changed = true
		}
	}
	return changed
}

// applySelf records what the introductions give this device: the folders it takes part in.
func (m *Membership) applySelf(cfg *config.Config, gs []grant.Grant) bool {
	type want struct {
		relay  bool
		issuer string
		seq    int64
	}
	desired := map[string]want{}
	for _, g := range gs {
		if g.Revoked {
			continue
		}
		for _, sh := range g.Folders {
			cur, seen := desired[sh.ID]
			better := !seen || (cur.relay && !sh.Relay) ||
				(cur.relay == sh.Relay && !established(cfg, cur.issuer) && established(cfg, g.Issuer))
			if better {
				cur.relay, cur.issuer = sh.Relay, g.Issuer
			}
			cur.seq = max(cur.seq, g.Seq)
			desired[sh.ID] = cur
		}
	}
	changed := false
	for _, id := range sortedKeys(desired) {
		w := desired[id]
		if cfg.Folder(id) != nil || cfg.NetworkDir == "" {
			continue // already here, or nowhere to put it (reported by the status page)
		}
		if seq, ok := cfg.Dismissed["folder/"+id]; ok && w.seq <= seq {
			continue // the owner removed it after that grant
		}
		// A full copy is created only on the word of a device known to hold full copies:
		// its secret will be fetched from it, and a relay must never receive it.
		if !w.relay && !established(cfg, w.issuer) {
			m.log.Warn("introduced folder skipped: the introducer is not known to hold full copies", "folder", id)
			continue
		}
		path := filepath.Join(cfg.NetworkDir, id)
		role := config.RoleFull
		if w.relay {
			role = config.RoleRelay
		}
		cfg.Folders = append(cfg.Folders, config.Folder{
			ID: id, Label: id, Path: path, Role: role, Managed: true, AwaitingSecret: role == config.RoleFull,
			// The device that introduced the folder is a member of it: that is where we get it from.
			Devices: []config.FolderDevice{{ID: w.issuer, Managed: true}},
		})
		if cfg.Device(w.issuer) == nil {
			cfg.Devices = append(cfg.Devices, config.Device{ID: w.issuer, Managed: true})
		}
		changed = true
	}
	// Folders no longer introduced, when a member of them says so (see applyOthers). The
	// files stay where they are.
	heardAny := false
	for i := 0; i < len(cfg.Folders); i++ {
		if f := cfg.Folders[i]; f.Managed {
			if _, still := desired[f.ID]; !still && heardFrom(&f, gs, m.id.ID) {
				heardAny = true
				m.log.Info("folder withdrawn by the device that introduced it; its files are left untouched", "folder", f.ID, "path", f.Path)
				cfg.Folders = slices.Delete(cfg.Folders, i, i+1)
				i--
				changed = true
			}
		}
	}
	if len(desired) == 0 && heardAny { // removed from the network: drop what was introduced
		for _, d := range slices.Clone(cfg.Devices) {
			if d.Managed && withdraw(cfg, d.ID, nil) {
				changed = true
			}
		}
	}
	return changed
}

// ---- issuing --------------------------------------------------------------------------------

// IssueFromConfig writes the signed introductions that describe this device's own
// configuration: every device it was paired with directly (or set up by hand),
// limited to the folders this device holds a full copy of. A device that disappeared
// from the configuration is revoked. Devices that arrived through somebody else's
// introduction are not re-issued (two devices would otherwise keep answering each
// other). A relay holds no full copy and so introduces nobody. It reports whether
// anything was issued.
func (m *Membership) IssueFromConfig(cfg *config.Config) (issued bool, err error) {
	now := time.Now()
	want := map[string]grant.Grant{}
	for _, d := range cfg.Devices {
		if d.Managed || d.ID == m.id.ID {
			continue
		}
		g := grant.Grant{Subject: d.ID, Name: d.Name}
		for _, f := range cfg.Folders {
			if f.Role != config.RoleFull || f.AwaitingSecret || !f.SharesWith(d.ID) {
				continue
			}
			g.Folders = append(g.Folders, grant.Share{ID: f.ID, Relay: f.IsRelay(d.ID)})
			// A full device paired with this one may introduce others in turn.
			g.Introducer = g.Introducer || !f.IsRelay(d.ID)
		}
		if len(g.Folders) == 0 {
			continue
		}
		want[d.ID] = g
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := want[id]
		if cur, ok := m.set.By(m.id.ID, id); ok && cur.SameRights(normalised(g)) {
			continue // already said
		}
		if _, _, err := m.set.Issue(m.id.Key, g, now); err != nil {
			return issued, fmt.Errorf("introduction of %s: %w", id[:min(len(id), 12)], err)
		}
		issued = true
	}
	// Revoke what this device once introduced and no longer has.
	for _, e := range m.set.All() {
		g := e.Grant
		if g.Issuer != m.id.ID || g.Revoked {
			continue
		}
		if _, still := want[g.Subject]; still {
			continue
		}
		if d := cfg.Device(g.Subject); d != nil && d.Managed {
			continue // now described by somebody else
		}
		if _, _, err := m.set.Issue(m.id.Key, grant.Grant{Subject: g.Subject, Revoked: true}, now); err != nil {
			return issued, err
		}
		issued = true
	}
	if issued {
		err = m.set.Save()
	}
	return issued, err
}

// normalised sorts the folder list the way Sign does, for comparing rights.
func normalised(g grant.Grant) grant.Grant {
	if g.Folders == nil {
		g.Folders = []grant.Share{}
	}
	g.Name = config.Clean(g.Name)
	sort.Slice(g.Folders, func(i, j int) bool { return g.Folders[i].ID < g.Folders[j].ID })
	return g
}

// ---- what to tell a connected device -----------------------------------------------------

// ForPeer selects the grants worth sending to a connected device: its own, those of the
// devices that share a folder with it, those that used to (so that being narrowed or
// demoted reaches the devices that knew the old state), every revocation, and the
// introductions that make an issuer of those trusted (so the peer can check the chain).
// Devices it shares nothing with are not revealed.
func (m *Membership) ForPeer(cfg *config.Config, peerID string) []grant.Signed {
	trusted := m.set.Trusted(pinned(cfg, m.id.ID))
	mine := map[string]bool{}
	for _, f := range cfg.Folders {
		if f.SharesWith(peerID) {
			mine[f.ID] = true
		}
	}
	var all []grant.Entry
	for _, e := range m.set.All() {
		if trusted[e.Grant.Issuer] {
			all = append(all, e)
		}
	}
	chosen := make([]bool, len(all))
	for i, e := range all {
		g := e.Grant
		chosen[i] = g.Subject == peerID || e.PrevIntroducer || g.Revoked || e.Mentions(mine)
	}
	for round := 0; round < 8; round++ { // follow the chain of introducers back to someone the peer trusts
		added := false
		for i, e := range all {
			if !chosen[i] || e.Grant.Issuer == m.id.ID {
				continue
			}
			for j, link := range all {
				l := link.Grant
				if !chosen[j] && l.Subject == e.Grant.Issuer && l.Introducer && !l.Revoked {
					chosen[j], added = true, true
				}
			}
		}
		if !added {
			break
		}
	}
	var out []grant.Signed
	for i, e := range all {
		if chosen[i] {
			out = append(out, e.Signed)
		}
	}
	return out
}

// Dismiss records that the owner removed a device by hand: only a grant newer than
// the current ones brings it back.
func (m *Membership) Dismiss(cfg *config.Config, deviceID string) {
	if g, ok := m.set.Latest(deviceID); ok {
		if cfg.Dismissed == nil {
			cfg.Dismissed = map[string]int64{}
		}
		cfg.Dismissed[deviceID] = max(cfg.Dismissed[deviceID], g.Seq)
	}
}

// DismissFolder records that the owner removed a folder that had been introduced: only a
// newer grant for this device recreates it.
func (m *Membership) DismissFolder(cfg *config.Config, folderID string) {
	if g, ok := m.set.Latest(m.id.ID); ok {
		if cfg.Dismissed == nil {
			cfg.Dismissed = map[string]int64{}
		}
		cfg.Dismissed["folder/"+folderID] = max(cfg.Dismissed["folder/"+folderID], g.Seq)
	}
}

// heardFrom reports whether any of the grants was issued by a member of the folder.
func heardFrom(f *config.Folder, gs []grant.Grant, self string) bool {
	for _, g := range gs {
		if _, member := roleIn(f, self, g.Issuer); member {
			return true
		}
	}
	return false
}
