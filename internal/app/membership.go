package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/grant"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// GrantsFile holds the signed grants a device has received or issued.
const GrantsFile = "grants.json"

// Membership connects the signed grants of the network's admins to this device's
// configuration.
//
// Two directions:
//   - receiving: a grant from a trusted admin says that device S takes part in
//     folders F. The matching entries are added to config.json, marked "managed" so
//     that a later grant (a revocation, a change) can take them out again. Entries
//     the owner created by hand are never touched.
//   - issuing (admins): the devices this admin was paired with directly are
//     described in a signed grant, so that the devices they share folders with can
//     learn about them without the owner connecting everything one by one.
//
// What a grant can change is limited on purpose:
//   - the issuer must be a trusted admin (pinned by the owner or promoted by one);
//   - only folders this device has are affected (a grant never creates access to
//     something the issuer is not a member of);
//   - a full copy is only honoured when the issuer is itself a full member of the
//     folder: a relay is blind (it never has the folder secret), and a blind
//     device that could introduce full copies would defeat that.
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

// pinned lists the admins the owner trusts directly (this device itself, if it is one).
func pinned(cfg *config.Config, self string) map[string]bool {
	p := map[string]bool{}
	for _, d := range cfg.Devices {
		if d.Admin {
			p[d.ID] = true
		}
	}
	if cfg.Admin {
		p[self] = true
	}
	return p
}

// ---- receiving ------------------------------------------------------------------------

// Receive takes grants offered by a connected device, keeps the valid ones and
// brings the configuration in line. It reports whether config.json changed (the
// daemon then restarts on its own).
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
	if !m.Apply(cfg) {
		return false
	}
	for id, addrs := range hints { // where to find the devices just introduced
		if d := cfg.Device(id); d != nil && d.Managed && len(d.Addresses) == 0 {
			d.Addresses = uniqueFirst(addrs, protocol.MaxHintAddrs)
		}
	}
	if err := SaveConfig(m.home, cfg); err != nil {
		m.log.Error("could not apply the network update", "err", err)
		return false
	}
	return true
}

// Apply brings cfg in line with the effective grants and reports whether it changed.
func (m *Membership) Apply(cfg *config.Config) bool {
	trusted := m.set.Trusted(pinned(cfg, m.id.ID))
	var eff []grant.Grant
	byID := map[string]grant.Grant{} // how each device describes its own role in a folder
	for _, e := range m.set.All() {
		if !trusted[e.Grant.Issuer] {
			continue
		}
		if e.Grant.Issuer == e.Grant.Subject {
			continue // self-descriptions are applied below, after the grants
		}
		if seq, ok := cfg.Dismissed[e.Grant.Subject]; ok && e.Grant.Seq <= seq {
			continue
		}
		eff = append(eff, e.Grant)
		byID[e.Grant.Subject] = e.Grant
	}
	for _, e := range m.set.All() {
		if e.Grant.Issuer == e.Grant.Subject && trusted[e.Grant.Issuer] && !e.Grant.Revoked {
			byID[e.Grant.Subject] = e.Grant
		}
	}
	changed := false
	// What this device is given first (it creates the folders), then the others.
	// A few passes: a device vouched for by an admin is only honoured once that
	// admin's own membership of the folder is known.
	for pass := 0; pass < 4; pass++ {
		progress := false
		for _, g := range eff {
			var ch bool
			if g.Subject == m.id.ID {
				ch = m.applySelf(cfg, g, byID)
			} else if g.Issuer != m.id.ID { // our own grants describe our configuration already
				ch = applyOther(cfg, g, m.id.ID, byID)
			}
			progress = progress || ch
		}
		changed = changed || progress
		if !progress {
			break
		}
	}
	return changed
}

// memberRole says whether device id takes part in folder f, and as a relay or not.
// For a folder this device has, its own configuration answers; for a folder it
// does not have yet, the device's own signed description does.
func memberRole(cfg *config.Config, self, id, folderID string, byID map[string]grant.Grant) (relay, ok bool) {
	if f := cfg.Folder(folderID); f != nil {
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
	if g, found := byID[id]; found && !g.Revoked {
		if sh, ok := g.Has(folderID); ok {
			return sh.Relay, true
		}
	}
	return false, false
}

// applyOther records what a grant says about another device.
func applyOther(cfg *config.Config, g grant.Grant, self string, byID map[string]grant.Grant) bool {
	changed := false
	if g.Revoked {
		return withdraw(cfg, g.Subject, nil)
	}
	keep := map[string]bool{}
	for _, sh := range g.Folders {
		f := cfg.Folder(sh.ID)
		if f == nil {
			continue
		}
		if f.Role == config.RoleRelay && sh.Relay {
			continue // relays do not talk to each other
		}
		issuerRelay, member := memberRole(cfg, self, g.Issuer, sh.ID, byID)
		if !member || (!sh.Relay && issuerRelay) {
			continue // not a member, or a blind device vouching for a full copy
		}
		keep[sh.ID] = true
		if ensureDevice(cfg, g) {
			changed = true
		}
		i := slices.IndexFunc(f.Devices, func(d config.FolderDevice) bool { return d.ID == g.Subject })
		switch {
		case i < 0:
			f.Devices = append(f.Devices, config.FolderDevice{ID: g.Subject, Relay: sh.Relay, Managed: true})
			changed = true
		case f.Devices[i].Managed && f.Devices[i].Relay != sh.Relay:
			f.Devices[i].Relay = sh.Relay
			changed = true
		}
	}
	return withdraw(cfg, g.Subject, keep) || changed
}

// ensureDevice makes sure the device is in the configuration; it reports a change.
func ensureDevice(cfg *config.Config, g grant.Grant) bool {
	if d := cfg.Device(g.Subject); d != nil {
		if d.Managed && g.Name != "" && d.Name != g.Name {
			d.Name = g.Name
			return true
		}
		return false
	}
	cfg.Devices = append(cfg.Devices, config.Device{ID: g.Subject, Name: g.Name, Managed: true})
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
	if d := cfg.Device(subject); d != nil && d.Managed && !d.Admin {
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

// applySelf records what the admins grant this device: the folders it takes part in.
func (m *Membership) applySelf(cfg *config.Config, g grant.Grant, byID map[string]grant.Grant) bool {
	if g.Revoked {
		return m.leave(cfg)
	}
	changed := false
	keep := map[string]bool{}
	for _, sh := range g.Folders {
		keep[sh.ID] = true
		if cfg.Folder(sh.ID) != nil {
			continue
		}
		if cfg.NetworkDir == "" {
			continue // nowhere to put it: reported by the status page
		}
		// The issuer must itself be a member, as shown by its own description.
		issuerRelay, member := memberRole(cfg, m.id.ID, g.Issuer, sh.ID, byID)
		if !member || (!sh.Relay && issuerRelay) {
			continue
		}
		path := filepath.Join(cfg.NetworkDir, sh.ID)
		probe := *cfg
		probe.Folders = append(slices.Clone(cfg.Folders), config.Folder{ID: sh.ID, Path: path})
		if err := probe.CheckPaths(m.home); err != nil {
			m.log.Warn("granted folder skipped", "folder", sh.ID, "err", err)
			continue
		}
		role := config.RoleFull
		if sh.Relay {
			role = config.RoleRelay
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			m.log.Warn("granted folder skipped", "folder", sh.ID, "err", err)
			continue
		}
		cfg.Folders = append(cfg.Folders, config.Folder{
			ID: sh.ID, Label: sh.ID, Path: path, Role: role, Managed: true, AwaitingSecret: role == config.RoleFull,
			// The admin that gave us the folder is a member of it: that is where we get it from.
			Devices: []config.FolderDevice{{ID: g.Issuer, Relay: issuerRelay, Managed: true}},
		})
		if cfg.Device(g.Issuer) == nil {
			cfg.Devices = append(cfg.Devices, config.Device{ID: g.Issuer, Managed: true})
		}
		changed = true
	}
	// Folders the admins no longer give us. The files stay where they are.
	for i := 0; i < len(cfg.Folders); i++ {
		if f := cfg.Folders[i]; f.Managed && !keep[f.ID] {
			m.log.Info("folder withdrawn by an admin; its files are left untouched", "folder", f.ID, "path", f.Path)
			cfg.Folders = slices.Delete(cfg.Folders, i, i+1)
			i--
			changed = true
		}
	}
	return changed
}

// leave drops everything an admin gave this device (hand-made entries stay).
func (m *Membership) leave(cfg *config.Config) bool {
	changed := false
	for i := 0; i < len(cfg.Folders); i++ {
		if cfg.Folders[i].Managed {
			m.log.Warn("removed from the network by an admin; folder dropped, files left untouched", "folder", cfg.Folders[i].ID)
			cfg.Folders = slices.Delete(cfg.Folders, i, i+1)
			i--
			changed = true
		}
	}
	for i := range cfg.Folders {
		f := &cfg.Folders[i]
		n := len(f.Devices)
		f.Devices = slices.DeleteFunc(f.Devices, func(d config.FolderDevice) bool { return d.Managed })
		changed = changed || n != len(f.Devices)
	}
	n := len(cfg.Devices)
	cfg.Devices = slices.DeleteFunc(cfg.Devices, func(d config.Device) bool { return d.Managed && !d.Admin })
	return changed || n != len(cfg.Devices)
}

// ---- issuing (admins) --------------------------------------------------------------------

// IssueFromConfig writes the signed grants that describe this admin's own
// configuration: itself (so others learn in which role it takes part in each
// folder), and every device it shares folders with that it added itself. A device
// that disappeared from the configuration is revoked. Devices that arrived through
// somebody else's grant are not re-issued (that would make two admins fight over
// the same grant forever). It reports whether anything was issued.
func (m *Membership) IssueFromConfig(cfg *config.Config) (issued bool, err error) {
	if !cfg.Admin {
		return false, nil
	}
	now := time.Now()
	want := map[string]grant.Grant{}
	// Itself.
	me := grant.Grant{Subject: m.id.ID, Name: cfg.DeviceName, Admin: true, Issuer: m.id.ID}
	for _, f := range cfg.Folders {
		me.Folders = append(me.Folders, grant.Share{ID: f.ID, Relay: f.Role == config.RoleRelay})
	}
	want[me.Subject] = me
	for _, d := range cfg.Devices {
		if d.Managed || d.ID == m.id.ID {
			continue
		}
		g := grant.Grant{Subject: d.ID, Name: d.Name, Admin: d.Admin}
		for _, f := range cfg.Folders {
			if f.SharesWith(d.ID) {
				g.Folders = append(g.Folders, grant.Share{ID: f.ID, Relay: f.IsRelay(d.ID)})
			}
		}
		if len(g.Folders) == 0 && !g.Admin {
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
		get := m.set.Get
		if id == m.id.ID {
			get = m.set.GetSelf
		}
		if cur, ok := get(id); ok && cur.SameRights(normalised(g)) {
			continue // already said (by this admin, or by another that says the same)
		}
		if _, _, err := m.set.Issue(m.id.Key, g, now); err != nil {
			return issued, fmt.Errorf("grant for %s: %w", id[:min(len(id), 12)], err)
		}
		issued = true
	}
	// Revoke what this admin once granted and no longer configures.
	for _, e := range m.set.All() {
		g := e.Grant
		if g.Issuer != m.id.ID || g.Revoked || g.Subject == m.id.ID {
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

// ForPeer selects the grants worth sending to a connected device: its own, those
// of the admins (it needs them to check the others), and those of the devices
// that share a folder with it. Anything else stays private to whoever shares it.
func (m *Membership) ForPeer(cfg *config.Config, peerID string) []grant.Signed {
	trusted := m.set.Trusted(pinned(cfg, m.id.ID))
	mine := map[string]bool{}
	for _, f := range cfg.Folders {
		if f.SharesWith(peerID) {
			mine[f.ID] = true
		}
	}
	var out []grant.Signed
	for _, e := range m.set.All() {
		g := e.Grant
		if !trusted[g.Issuer] {
			continue
		}
		send := g.Subject == peerID || trusted[g.Subject]
		for _, sh := range g.Folders {
			send = send || mine[sh.ID]
		}
		if g.Revoked {
			send = true // a revocation reaches everyone who might still list the device
		}
		if send {
			out = append(out, e.Signed)
		}
	}
	return out
}

// Dismiss records that the owner removed a device by hand: only a grant newer than
// the current one brings it back.
func (m *Membership) Dismiss(cfg *config.Config, deviceID string) {
	if g, ok := m.set.Get(deviceID); ok {
		if cfg.Dismissed == nil {
			cfg.Dismissed = map[string]int64{}
		}
		cfg.Dismissed[deviceID] = g.Seq
	}
}
