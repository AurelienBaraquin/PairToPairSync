package app

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/grant"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/seal"
)

func newID(t *testing.T) *identity.Identity {
	t.Helper()
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := peer.IDFromPrivateKey(k)
	return &identity.Identity{Key: k, PeerID: pid, ID: pid.String()}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// member builds a Membership for `self` in its own home with the given config.
func member(t *testing.T, self *identity.Identity, cfg *config.Config) *Membership {
	t.Helper()
	home := t.TempDir()
	cfg.DeviceName = "dev"
	if err := cfg.Save(filepath.Join(home, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	return LoadMembership(home, self, quietLog())
}

func signed(t *testing.T, by *identity.Identity, g grant.Grant) grant.Signed {
	t.Helper()
	sg, err := grant.Sign(by.Key, g, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

func folderCfg(t *testing.T, id, role string, devs ...config.FolderDevice) config.Folder {
	return config.Folder{ID: id, Path: filepath.Join(t.TempDir(), id), Role: role, Devices: devs}
}

func TestJoiningThroughAnAdminLearnsTheOtherDevices(t *testing.T) {
	admin, other, n := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	cfg.Devices = []config.Device{{ID: admin.ID, Name: "admin", Admin: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID})}
	cfg.Folders[0].Secret = "" // none needed: no relay
	m := member(t, n, cfg)

	list := []grant.Signed{
		// The admin describes itself (it is a full member of docs and photos) ...
		signed(t, admin, grant.Grant{Subject: admin.ID, Name: "admin", Admin: true,
			Folders: []grant.Share{{ID: "docs"}, {ID: "photos"}}}),
		// ... and vouches for this device (docs + photos) and for another one (docs only).
		signed(t, admin, grant.Grant{Subject: n.ID, Name: "me", Folders: []grant.Share{{ID: "docs"}, {ID: "photos"}}}),
		signed(t, admin, grant.Grant{Subject: other.ID, Name: "laptop", Folders: []grant.Share{{ID: "docs"}}}),
	}
	if !m.recv(admin.ID, list) {
		t.Fatal("nothing changed")
	}
	got, err := LoadConfig(m.home)
	if err != nil {
		t.Fatal(err)
	}
	d := got.Device(other.ID)
	if d == nil || !d.Managed || d.Name != "laptop" {
		t.Fatalf("the other device was not learnt: %+v", d)
	}
	if f := got.Folder("docs"); f == nil || !f.SharesWith(other.ID) || f.IsRelay(other.ID) {
		t.Fatalf("docs not shared with the other device: %+v", f)
	}
	ph := got.Folder("photos")
	if ph == nil || !ph.Managed || !ph.AwaitingSecret || ph.Role != config.RoleFull || !ph.SharesWith(admin.ID) {
		t.Fatalf("granted folder not created as expected: %+v", ph)
	}
	if ph.SharesWith(other.ID) {
		t.Fatal("a device that was not granted photos got it")
	}
	if filepath.Dir(ph.Path) != cfg.NetworkDir {
		t.Fatalf("folder created outside the network directory: %s", ph.Path)
	}
}

func TestAGrantFromAStrangerChangesNothing(t *testing.T) {
	admin, stranger, victim := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID})}
	m := member(t, newID(t), cfg)
	sg := signed(t, stranger, grant.Grant{Subject: victim.ID, Folders: []grant.Share{{ID: "docs"}}})
	if m.recv(stranger.ID, []grant.Signed{sg}) {
		t.Fatal("a grant by an unknown device was applied")
	}
}

func TestAdminCannotGrantAFolderItIsNotAMemberOf(t *testing.T) {
	admin, x := newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}}
	// The admin is pinned, but is not a member of "private" here.
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID}), folderCfg(t, "private", config.RoleFull)}
	m := member(t, newID(t), cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: x.ID, Folders: []grant.Share{{ID: "docs"}, {ID: "private"}}})})
	got, _ := LoadConfig(m.home)
	if !got.Folder("docs").SharesWith(x.ID) {
		t.Fatal("docs should have been shared")
	}
	if got.Folder("private").SharesWith(x.ID) {
		t.Fatal("an admin that is not a member of a folder granted access to it")
	}
}

// A relay never has the folder secret. If it could introduce full copies, whoever
// controls the relay (a lost phone) could obtain the secret from the full devices.
func TestBlindAdminCanOnlyIntroduceRelays(t *testing.T) {
	relayAdmin, full, relay2 := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: relayAdmin.ID, Admin: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: relayAdmin.ID, Relay: true})}
	sec, _ := seal.NewSecret()
	cfg.Folders[0].Secret = sec // a folder with a relay needs its secret
	m := member(t, newID(t), cfg)
	m.recv(relayAdmin.ID, []grant.Signed{
		signed(t, relayAdmin, grant.Grant{Subject: full.ID, Folders: []grant.Share{{ID: "docs"}}}),                // a full copy
		signed(t, relayAdmin, grant.Grant{Subject: relay2.ID, Folders: []grant.Share{{ID: "docs", Relay: true}}}), // a relay
	})
	got, _ := LoadConfig(m.home)
	if got.Folder("docs").SharesWith(full.ID) {
		t.Fatal("a relay-only admin introduced a full copy")
	}
	if !got.Folder("docs").IsRelay(relay2.ID) {
		t.Fatal("a relay-only admin should be able to introduce a relay")
	}
}

func TestRevocationRemovesOnlyWhatGrantsAdded(t *testing.T) {
	admin, byGrant, byHand := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}, {ID: byHand.ID, Name: "hand"}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID}, config.FolderDevice{ID: byHand.ID})}
	m := member(t, newID(t), cfg)

	both := []grant.Signed{
		signed(t, admin, grant.Grant{Subject: byGrant.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 10}),
		signed(t, admin, grant.Grant{Subject: byHand.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 10}),
	}
	m.recv(admin.ID, both)
	if got, _ := LoadConfig(m.home); !got.Folder("docs").SharesWith(byGrant.ID) {
		t.Fatal("grant not applied")
	}
	m.recv(admin.ID, []grant.Signed{
		signed(t, admin, grant.Grant{Subject: byGrant.ID, Revoked: true, Seq: 11}),
		signed(t, admin, grant.Grant{Subject: byHand.ID, Revoked: true, Seq: 11}),
	})
	got, _ := LoadConfig(m.home)
	if got.Folder("docs").SharesWith(byGrant.ID) || got.Device(byGrant.ID) != nil {
		t.Fatal("revoked device still configured")
	}
	if !got.Folder("docs").SharesWith(byHand.ID) || got.Device(byHand.ID) == nil {
		t.Fatal("a device paired by hand was removed by a revocation")
	}
}

func TestRemovedByHandStaysRemovedUntilANewerGrant(t *testing.T) {
	admin, x := newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID})}
	m := member(t, newID(t), cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: x.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 100})})

	// The owner removes the device by hand.
	WithConfigLock(func() {
		c, _ := LoadConfig(m.home)
		withdraw(c, x.ID, nil)
		m.Dismiss(c, x.ID)
		if err := SaveConfig(m.home, c); err != nil {
			t.Fatal(err)
		}
	})
	if m.Reconcile() {
		t.Fatal("a device removed by hand came back from the same grant")
	}
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: x.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 200})})
	if got, _ := LoadConfig(m.home); !got.Folder("docs").SharesWith(x.ID) {
		t.Fatal("a newer grant should bring the device back")
	}
}

func TestBeingRevokedDropsGrantedFoldersButKeepsTheFiles(t *testing.T) {
	admin, n := newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "mine", config.RoleFull)}
	m := member(t, n, cfg)
	self := signed(t, admin, grant.Grant{Subject: admin.ID, Admin: true, Folders: []grant.Share{{ID: "docs"}}})
	m.recv(admin.ID, []grant.Signed{self, signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 5})})
	if got, _ := LoadConfig(m.home); got.Folder("docs") == nil {
		t.Fatal("folder not created")
	}
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Revoked: true, Seq: 6})})
	got, _ := LoadConfig(m.home)
	if got.Folder("docs") != nil {
		t.Fatal("granted folder still configured after revocation")
	}
	if got.Folder("mine") == nil {
		t.Fatal("a folder created by hand was dropped")
	}
	if got.Device(admin.ID) == nil || !got.Device(admin.ID).Admin {
		t.Fatal("the pinned admin must stay: that was the owner's choice")
	}
}

func TestAdminIssuesGrantsFromItsOwnConfig(t *testing.T) {
	a, b, c := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Admin = true
	cfg.Devices = []config.Device{{ID: b.ID, Name: "b"}, {ID: c.ID, Name: "c", Managed: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: b.ID}, config.FolderDevice{ID: c.ID, Managed: true}),
		folderCfg(t, "pics", config.RoleFull, config.FolderDevice{ID: b.ID, Relay: true})}
	m := member(t, a, cfg)

	issued, err := m.IssueFromConfig(cfg)
	if err != nil || !issued {
		t.Fatalf("issue: %v %v", issued, err)
	}
	gb, ok := m.set.Get(b.ID)
	if !ok || len(gb.Folders) != 2 || gb.Folders[1] != (grant.Share{ID: "pics", Relay: true}) {
		t.Fatalf("grant for b: %+v", gb)
	}
	if _, ok := m.set.Get(c.ID); ok {
		t.Fatal("a device that came from somebody else's grant must not be re-issued")
	}
	if me, ok := m.set.GetSelf(a.ID); !ok || len(me.Folders) != 2 {
		t.Fatalf("self description: %+v", me)
	}
	// Nothing changed: nothing issued, so no churn.
	if again, _ := m.IssueFromConfig(cfg); again {
		t.Fatal("grants re-issued without any change")
	}
	// The device is removed from this admin's configuration: it is revoked.
	cfg.Devices = cfg.Devices[1:]
	for i := range cfg.Folders {
		cfg.Folders[i].Devices = cfg.Folders[i].Devices[1:]
	}
	if _, err := m.IssueFromConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if g, _ := m.set.Get(b.ID); !g.Revoked {
		t.Fatal("a device removed by its admin was not revoked")
	}
}

func TestOnlyAdminsIssue(t *testing.T) {
	a, b := newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: b.ID}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: b.ID})}
	m := member(t, a, cfg)
	if issued, _ := m.IssueFromConfig(cfg); issued {
		t.Fatal("a device that is not an admin issued grants")
	}
}

func TestForPeerOnlyRevealsFolderMates(t *testing.T) {
	a, p, mate, stranger := newID(t), newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Admin = true
	cfg.Devices = []config.Device{{ID: p.ID}, {ID: mate.ID}, {ID: stranger.ID}}
	cfg.Folders = []config.Folder{
		folderCfg(t, "shared", config.RoleFull, config.FolderDevice{ID: p.ID}, config.FolderDevice{ID: mate.ID}),
		folderCfg(t, "other", config.RoleFull, config.FolderDevice{ID: stranger.ID}),
	}
	m := member(t, a, cfg)
	if _, err := m.IssueFromConfig(cfg); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sg := range m.ForPeer(cfg, p.ID) {
		g, _ := grant.Verify(sg)
		seen[g.Subject] = true
	}
	if !seen[p.ID] || !seen[mate.ID] || !seen[a.ID] {
		t.Fatalf("missing grants: %v", seen)
	}
	if seen[stranger.ID] {
		t.Fatal("a device the peer shares nothing with was revealed")
	}
}

func (m *Membership) recv(from string, list []grant.Signed) bool { return m.Receive(from, list, nil) }
