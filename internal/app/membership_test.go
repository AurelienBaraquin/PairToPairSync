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
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
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
		// The admin vouches for this device (docs + photos) and for another one (docs only).
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
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true, Full: true}}
	cfg.Folders = []config.Folder{folderCfg(t, "mine", config.RoleFull)}
	m := member(t, n, cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 5})})
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
	gb, ok := m.set.By(a.ID, b.ID)
	if !ok || len(gb.Folders) != 2 || gb.Folders[1] != (grant.Share{ID: "pics", Relay: true}) {
		t.Fatalf("grant for b: %+v", gb)
	}
	if _, ok := m.set.By(a.ID, c.ID); ok {
		t.Fatal("a device that came from somebody else's grant must not be re-issued")
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
	if g, _ := m.set.By(a.ID, b.ID); !g.Revoked {
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
	if !seen[p.ID] || !seen[mate.ID] {
		t.Fatalf("missing grants: %v", seen)
	}
	if seen[stranger.ID] {
		t.Fatal("a device the peer shares nothing with was revealed")
	}
}

func (m *Membership) recv(from string, list []grant.Signed) bool { return m.Receive(from, list, nil) }

// ---- regressions found by the independent review ------------------------------------------

// A relay-only admin used to be able to claim, about itself, to be a full member of a
// folder the receiver does not have yet, and so get a folder whose secret would later be
// handed to it.
func TestBlindAdminCannotCreateAFullFolderByClaimingToHoldIt(t *testing.T) {
	relayAdmin, n := newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	// Pinned by the owner, known only as a relay of an existing folder.
	cfg.Devices = []config.Device{{ID: relayAdmin.ID, Admin: true}}
	sec, _ := seal.NewSecret()
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: relayAdmin.ID, Relay: true})}
	cfg.Folders[0].Secret = sec
	m := member(t, n, cfg)
	m.recv(relayAdmin.ID, []grant.Signed{signed(t, relayAdmin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "Documents"}}})})
	got, _ := LoadConfig(m.home)
	if got.Folder("Documents") != nil {
		t.Fatal("a folder was created on the word of a device that is not known to hold full copies")
	}
	if _, ok := secretFrom(got, "Documents", relayAdmin.ID); ok {
		t.Fatal("secret released")
	}
	// The same admin may hand out a relay role, which carries no secret.
	m.recv(relayAdmin.ID, []grant.Signed{signed(t, relayAdmin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "Documents", Relay: true}}, Seq: time.Now().UnixNano() + 5})})
	if got, _ := LoadConfig(m.home); got.Folder("Documents") == nil || got.Folder("Documents").Role != config.RoleRelay {
		t.Fatal("a relay folder should be creatable")
	}
}

// secretFrom mimics App.secretFor on a configuration.
func secretFrom(cfg *config.Config, folderID, requester string) (string, bool) {
	a := &App{Cfg: cfg}
	return a.secretFor(requester, folderID)
}

func TestAnAdminHeldAsFullByTheOwnerCanCreateFullFolders(t *testing.T) {
	admin, n := newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true, Full: true}}
	m := member(t, n, cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "photos"}}})})
	got, _ := LoadConfig(m.home)
	if f := got.Folder("photos"); f == nil || !f.AwaitingSecret || !f.SharesWith(admin.ID) || f.IsRelay(admin.ID) {
		t.Fatalf("%+v", f)
	}
}

// Another admin narrowing its own grant must not erase what a different admin granted.
func TestAdminsGrantsAreUnioned(t *testing.T) {
	a, b, s := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: a.ID, Admin: true}, {ID: b.ID, Admin: true}}
	cfg.Folders = []config.Folder{
		folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: a.ID}, config.FolderDevice{ID: b.ID}),
		folderCfg(t, "photos", config.RoleFull, config.FolderDevice{ID: a.ID}),
	}
	m := member(t, newID(t), cfg)
	now := time.Now().UnixNano()
	m.recv(a.ID, []grant.Signed{signed(t, a, grant.Grant{Subject: s.ID, Folders: []grant.Share{{ID: "docs"}, {ID: "photos"}}, Seq: now})})
	m.recv(b.ID, []grant.Signed{signed(t, b, grant.Grant{Subject: s.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: now + 100})})
	got, _ := LoadConfig(m.home)
	if !got.Folder("photos").SharesWith(s.ID) {
		t.Fatal("one admin's narrower grant took away what the other had granted")
	}
	// When both withdraw, it goes.
	m.recv(a.ID, []grant.Signed{signed(t, a, grant.Grant{Subject: s.ID, Revoked: true, Seq: now + 200})})
	m.recv(b.ID, []grant.Signed{signed(t, b, grant.Grant{Subject: s.ID, Revoked: true, Seq: now + 300})})
	if got, _ := LoadConfig(m.home); got.Device(s.ID) != nil {
		t.Fatal("device kept after every admin revoked it")
	}
}

// A device that was narrowed to other folders must still be reported to the devices
// that only knew it through the folder it lost.
func TestNarrowingReachesTheDevicesThatKnewTheOldFolder(t *testing.T) {
	a, d1, d2, s := newID(t), newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Admin = true
	cfg.Devices = []config.Device{{ID: d1.ID}, {ID: d2.ID}, {ID: s.ID}}
	cfg.Folders = []config.Folder{
		folderCfg(t, "F", config.RoleFull, config.FolderDevice{ID: d1.ID}, config.FolderDevice{ID: s.ID}),
		folderCfg(t, "G", config.RoleFull, config.FolderDevice{ID: d2.ID}, config.FolderDevice{ID: s.ID}),
	}
	m := member(t, a, cfg)
	if _, err := m.IssueFromConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// The owner removes s from F.
	cfg.Folders[0].Devices = cfg.Folders[0].Devices[:1]
	if _, err := m.IssueFromConfig(cfg); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sg := range m.ForPeer(cfg, d1.ID) {
		if g, _ := grant.Verify(sg); g.Subject == s.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("d1 would keep s in F forever: it never hears about the narrowing")
	}
}

// One grant that cannot be applied must not stop the others (and a removal must still
// be applied when a hand-made folder mentions the device).
func TestAnUnappliableGrantDoesNotBlockTheRest(t *testing.T) {
	admin, good, bad := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}}
	// A hand-made full folder with no secret: adding a relay to it would be invalid.
	cfg.Folders = []config.Folder{
		folderCfg(t, "nosecret", config.RoleFull, config.FolderDevice{ID: admin.ID}),
		folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID}),
	}
	m := member(t, newID(t), cfg)
	m.recv(admin.ID, []grant.Signed{
		signed(t, admin, grant.Grant{Subject: bad.ID, Folders: []grant.Share{{ID: "nosecret", Relay: true}}}),
		signed(t, admin, grant.Grant{Subject: good.ID, Folders: []grant.Share{{ID: "docs"}}}),
	})
	got, _ := LoadConfig(m.home)
	if got.Folder("docs") == nil || !got.Folder("docs").SharesWith(good.ID) {
		t.Fatal("a valid grant was dropped because another one was not valid")
	}
	if got.Device(bad.ID) != nil {
		t.Fatal("the invalid grant was applied")
	}
}

func TestBeingRemovedWithAHandMadeFolderMentioningAManagedDevice(t *testing.T) {
	admin, n, other := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true, Full: true}, {ID: other.ID, Managed: true}}
	// The owner shared a hand-made folder with a device an admin introduced.
	cfg.Folders = []config.Folder{folderCfg(t, "mine", config.RoleFull, config.FolderDevice{ID: other.ID})}
	m := member(t, n, cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 5})})
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Revoked: true, Seq: 6})})
	got, err := LoadConfig(m.home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Folder("docs") != nil {
		t.Fatal("the revocation could not be applied because of a dangling reference")
	}
	if got.Folder("mine") == nil || got.Device(other.ID) == nil {
		t.Fatal("a device that a hand-made folder uses was dropped")
	}
}

func TestFolderRemovedByHandStaysRemoved(t *testing.T) {
	admin, n, other := newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.NetworkDir = t.TempDir()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true, Full: true}}
	m := member(t, n, cfg)
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 100})})
	WithConfigLock(func() {
		c, _ := LoadConfig(m.home)
		c.Folders = nil
		m.DismissFolder(c, "docs")
		if err := SaveConfig(m.home, c); err != nil {
			t.Fatal(err)
		}
	})
	// An unrelated update arrives: the folder must not come back.
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: other.ID, Folders: []grant.Share{{ID: "x"}}})})
	if got, _ := LoadConfig(m.home); got.Folder("docs") != nil {
		t.Fatal("a folder removed by hand was recreated by an unrelated update")
	}
	m.recv(admin.ID, []grant.Signed{signed(t, admin, grant.Grant{Subject: n.ID, Folders: []grant.Share{{ID: "docs"}}, Seq: 200})})
	if got, _ := LoadConfig(m.home); got.Folder("docs") == nil {
		t.Fatal("a newer grant should recreate it")
	}
}

func TestHintsAreFilteredAndOnlyTrustedAdminsWriteThem(t *testing.T) {
	node.AllowLoopbackHints = false
	t.Cleanup(func() { node.AllowLoopbackHints = true })
	got := node.CleanHints([]string{
		"203.0.113.7:22100", "192.168.1.5:22100", "/ip4/198.51.100.2/udp/22100/quic-v1",
		"evil.example:22100", "/dns4/evil.example/tcp/1", "127.0.0.1:22100", "/ip4/0.0.0.0/tcp/1", "[::1]:9", "224.0.0.1:5",
	})
	want := []string{"203.0.113.7:22100", "192.168.1.5:22100", "/ip4/198.51.100.2/udp/22100/quic-v1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// Hints relayed by a device that is not an admin are not written to config.json.
	admin, relayed, n, nobody := newID(t), newID(t), newID(t), newID(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: admin.ID, Admin: true}, {ID: nobody.ID}}
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull, config.FolderDevice{ID: admin.ID}, config.FolderDevice{ID: nobody.ID})}
	m := member(t, n, cfg)
	grants := []grant.Signed{signed(t, admin, grant.Grant{Subject: relayed.ID, Folders: []grant.Share{{ID: "docs"}}})}
	m.Receive(nobody.ID, grants, map[string][]string{relayed.ID: {"203.0.113.9:22100"}})
	if d := func() *config.Device { c, _ := LoadConfig(m.home); return c.Device(relayed.ID) }(); d == nil || len(d.Addresses) != 0 {
		t.Fatalf("hints from a non-admin were stored: %+v", d)
	}
}

func TestAGrantNamingItsOwnSubjectAsIssuerIsRefused(t *testing.T) {
	a := newID(t)
	if _, err := grant.Sign(a.Key, grant.Grant{Subject: a.ID, Folders: []grant.Share{{ID: "x"}}}, time.Now()); err == nil {
		t.Fatal("self-grant signed")
	}
}

func TestFolderIDsThatWindowsWouldMangleAreRefused(t *testing.T) {
	for _, bad := range []string{"con", "NUL", "aux.txt", "com1", "LPT9", "x.", "docs.", "a..b"} {
		if config.ValidFolderID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"docs", "Photos-2026", "a.b", "console", "com10", "my_folder"} {
		if !config.ValidFolderID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}

// Two guests approved around the same time: nothing is stored before a guest is really
// recorded (no ghost grant), and recording one never revokes the other.
func TestConcurrentApprovalsDoNotRevokeEachOther(t *testing.T) {
	admin := newID(t)
	cfg := config.Default()
	cfg.Admin = true
	cfg.Folders = []config.Folder{folderCfg(t, "docs", config.RoleFull)}
	sec, _ := seal.NewSecret()
	cfg.Folders[0].Secret = sec
	m := member(t, admin, cfg)
	p := newPairer(m.home, quietLog(), m)

	invite := func() string {
		inv, err := CreateNetworkInvite(m.home, cfg, admin.ID, []string{"/ip4/203.0.113.1/udp/1/quic-v1"}, NetworkInviteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return inv.Token
	}
	g1, g2 := newID(t), newID(t)
	accept := func(g *identity.Identity, token string) (resp protocol.PairResponse, commit func()) {
		return p.Accept(g.PeerID, "", protocol.PairRequest{Token: token, Name: "guest"}, func(time.Duration) {})
	}
	r1, c1 := accept(g1, invite())
	r2, c2 := accept(g2, invite())
	if !r1.OK || !r2.OK || c1 == nil || c2 == nil {
		t.Fatalf("accept failed: %+v %+v", r1, r2)
	}
	if _, ok := m.set.By(admin.ID, g1.ID); ok {
		t.Fatal("a grant was stored before the guest was recorded")
	}
	if len(r1.Grants) == 0 || r1.Secrets["docs"] != sec {
		t.Fatalf("the guest did not receive what it needs: %+v", r1)
	}
	c1()
	c2()
	for _, g := range []*identity.Identity{g1, g2} {
		if got, ok := m.set.By(admin.ID, g.ID); !ok || got.Revoked || len(got.Folders) != 1 {
			t.Fatalf("grant for %s: %+v (%v)", g.ID[:8], got, ok)
		}
	}
}
