package app_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// edit changes a device's configuration on disk the way the CLI does.
func (d *dev) edit(t *testing.T, fn func(*config.Config)) {
	t.Helper()
	app.WithConfigLock(func() {
		cfg, err := app.LoadConfig(d.home)
		if err != nil {
			t.Fatal(err)
		}
		fn(cfg)
		if err := app.SaveConfig(d.home, cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func (d *dev) config(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := app.LoadConfig(d.home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// joinNetwork makes g scan a network invitation, waits for the inviter to approve it,
// and returns g's configuration after joining.
func joinNetwork(t *testing.T, admin, g *dev, gcfg *config.Config, inv pairing.Invite, approve bool) (protocol.PairResponse, error) {
	t.Helper()
	type result struct {
		resp protocol.PairResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		r, err := pairing.Dial(ctx, g.id.Key, inv, app.GuestRequest(gcfg))
		done <- result{r, err}
	}()
	if inv.Approve {
		var id string
		eventually(t, "the request reached the inviter", func() bool {
			rs := admin.a.Requests()
			if len(rs) == 0 {
				return false
			}
			id = rs[0].ID
			return true
		})
		if rs := admin.a.Requests(); rs[0].DeviceID != g.id.ID {
			t.Fatalf("request shows the wrong device: %+v", rs[0])
		}
		if !admin.a.Decide(id, approve) {
			t.Fatal("request vanished")
		}
	}
	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(45 * time.Second):
		t.Fatal("pairing did not finish")
		return protocol.PairResponse{}, nil
	}
}

// networkOf builds device A and its existing full device B, paired by hand the old way.
func networkOf(t *testing.T) (root string, a, b *dev) {
	root = t.TempDir()
	a, b = newDev(t, root, "A", false), newDev(t, root, "B", false)
	a.start(t, b)
	b.start(t, a)
	return
}

func TestJoiningThroughOneDeviceConnectsToEveryDevice(t *testing.T) {
	root, a, b := networkOf(t)
	n, ncfg := freshGuest(t, root, "N")

	cfg := a.config(t)
	inv, err := app.CreateNetworkInvite(a.home, cfg, a.id.ID, a.a.Node.Addrs(), app.NetworkInviteOptions{Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	if inv, err = pairing.Decode(inv.Encode()); err != nil || !inv.Net || !inv.Approve {
		t.Fatalf("invitation lost its meaning in the QR code: %+v %v", inv, err)
	}
	resp, err := joinNetwork(t, a, n, ncfg, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Secrets["docs"] != testSecret {
		t.Fatal("the full guest did not receive the folder secret")
	}
	nm := app.LoadMembership(n.home, n.id, quietLog())
	app.WithConfigLock(func() {
		if err := app.JoinNetwork(nm, ncfg, inv, resp, app.JoinOptions{Dir: n.dir}); err != nil {
			t.Fatal(err)
		}
		if err := app.SaveConfig(n.home, ncfg); err != nil {
			t.Fatal(err)
		}
	})

	// N knows A (trusted) and, without ever being paired with it, B.
	got := n.config(t)
	if d := got.Device(a.id.ID); d == nil || !d.Full {
		t.Fatalf("the inviter is not trusted: %+v", d)
	}
	if d := got.Device(b.id.ID); d == nil || !d.Managed || len(d.Addresses) == 0 {
		t.Fatalf("N did not learn B (with where to find it): %+v", d)
	}
	f := got.Folder("docs")
	if f == nil || f.Secret != testSecret || f.AwaitingSecret || !f.Managed || !f.SharesWith(b.id.ID) {
		t.Fatalf("folder not set up: %+v", f)
	}
	if filepath.Dir(f.Path) != n.dir {
		t.Fatalf("folder created in the wrong place: %s", f.Path)
	}
	n.dir = f.Path // from here on the helpers read and write the folder itself

	// The inviter records the new device; restarting makes it tell B.
	waitConfigHas(t, a, n)
	a.reload(t)
	eventually(t, "B learnt about N from the inviter", func() bool {
		c, err := app.LoadConfig(b.home)
		return err == nil && c.Device(n.id.ID) != nil && c.Folder("docs").SharesWith(n.id.ID)
	})
	b.reload(t)
	n.reload(t)

	// Files flow between N and B directly: stop the inviter to prove it.
	a.stop()
	eventually(t, "N and B connected without the inviter", n.connectedTo(b))
	b.write(t, "from-b.txt", []byte("hello N"))
	eventually(t, "B to N", n.hasContent("from-b.txt", []byte("hello N")))
	n.write(t, "from-n.txt", []byte("hello B"))
	eventually(t, "N to B", b.hasContent("from-n.txt", []byte("hello B")))
}

func TestOwnerCanDeclineARequest(t *testing.T) {
	root, a, _ := networkOf(t)
	n, ncfg := freshGuest(t, root, "N")
	inv, err := app.CreateNetworkInvite(a.home, a.config(t), a.id.ID, a.a.Node.Addrs(), app.NetworkInviteOptions{Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := joinNetwork(t, a, n, ncfg, inv, false)
	if err == nil || len(resp.Grants) != 0 || len(resp.Secrets) != 0 {
		t.Fatalf("a declined device got an answer: %+v %v", resp, err)
	}
	time.Sleep(time.Second)
	if a.config(t).Device(n.id.ID) != nil {
		t.Fatal("a declined device was recorded")
	}
	if len(a.a.Requests()) != 0 {
		t.Fatal("the request stayed pending")
	}
}

func TestRelayCannotInviteIntoTheNetwork(t *testing.T) {
	root := t.TempDir()
	r := newDev(t, root, "R", true)
	r.start(t)
	cfg := r.config(t)
	if _, err := app.CreateNetworkInvite(r.home, cfg, r.id.ID, r.a.Node.Addrs(), app.NetworkInviteOptions{}); err == nil {
		t.Fatal("a relay, which holds no full copy, created a network invitation")
	}
}

func TestRevokedDeviceDisappearsFromTheOthers(t *testing.T) {
	root, a, b := networkOf(t)
	n, ncfg := freshGuest(t, root, "N")
	inv, err := app.CreateNetworkInvite(a.home, a.config(t), a.id.ID, a.a.Node.Addrs(), app.NetworkInviteOptions{Approve: false})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := joinNetwork(t, a, n, ncfg, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	nm := app.LoadMembership(n.home, n.id, quietLog())
	app.WithConfigLock(func() {
		if err := app.JoinNetwork(nm, ncfg, inv, resp, app.JoinOptions{Dir: n.dir}); err != nil {
			t.Fatal(err)
		}
		_ = app.SaveConfig(n.home, ncfg)
	})
	waitConfigHas(t, a, n)
	a.reload(t)
	eventually(t, "B learnt about N", func() bool { c, err := app.LoadConfig(b.home); return err == nil && c.Device(n.id.ID) != nil })
	b.reload(t)

	// The inviter removes N (what `p2psync device remove` does).
	a.edit(t, func(c *config.Config) {
		c.Devices = filterDevices(c.Devices, n.id.ID)
		for i := range c.Folders {
			c.Folders[i].Devices = filterFolderDevices(c.Folders[i].Devices, n.id.ID)
		}
	})
	a.reload(t) // issues the revocation
	eventually(t, "B dropped N", func() bool {
		c, err := app.LoadConfig(b.home)
		return err == nil && c.Device(n.id.ID) == nil && !c.Folder("docs").SharesWith(n.id.ID)
	})
}

func filterDevices(in []config.Device, id string) (out []config.Device) {
	for _, d := range in {
		if d.ID != id {
			out = append(out, d)
		}
	}
	return
}

func filterFolderDevices(in []config.FolderDevice, id string) (out []config.FolderDevice) {
	for _, d := range in {
		if d.ID != id {
			out = append(out, d)
		}
	}
	return
}

// A full device waiting for a folder's secret gets it from a connected full
// member, and a relay or an unlisted device cannot.
func TestSecretComesFromAFullMemberOnly(t *testing.T) {
	root := t.TempDir()
	b, n := newDev(t, root, "B", false), newDev(t, root, "N", false)
	b.start(t, n)
	n.start(t, b)
	// N is told about the folder but does not have its secret yet.
	n.edit(t, func(c *config.Config) {
		c.Folders[0].Secret, c.Folders[0].AwaitingSecret = "", true
	})
	n.reload(t)
	eventually(t, "the secret arrived and the folder started", func() bool {
		c, err := app.LoadConfig(n.home)
		return err == nil && c.Folders[0].Secret == testSecret && !c.Folders[0].AwaitingSecret
	})

	// B refuses a device it lists as a relay, and one it does not know.
	b.edit(t, func(c *config.Config) { c.Folders[0].Devices[0].Relay = true })
	b.reload(t)
	if _, err := n.a.Node.FetchSecret(context.Background(), b.id.ID, "docs"); err == nil {
		t.Fatal("a relay obtained the folder secret")
	}
	if _, err := n.a.Node.FetchSecret(context.Background(), b.id.ID, "nope"); err == nil {
		t.Fatal("a secret was given for an unknown folder")
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A phone joins as a relay: it never receives the secret, the other full device
// learns that it is a relay (not a full copy), and the relay cannot introduce anybody.
func TestPhoneJoinsAsRelayAndBlindnessHolds(t *testing.T) {
	root, a, b := networkOf(t)
	phone, pcfg := freshGuest(t, root, "Phone")

	inv, err := app.CreateNetworkInvite(a.home, a.config(t), a.id.ID, a.a.Node.Addrs(), app.NetworkInviteOptions{AsRelay: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := joinNetwork(t, a, phone, pcfg, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Secrets) != 0 || resp.Secret != "" {
		t.Fatal("a relay was given a folder secret")
	}
	pm := app.LoadMembership(phone.home, phone.id, quietLog())
	app.WithConfigLock(func() {
		if err := app.JoinNetwork(pm, pcfg, inv, resp, app.JoinOptions{Dir: phone.dir}); err != nil {
			t.Fatal(err)
		}
		if err := app.SaveConfig(phone.home, pcfg); err != nil {
			t.Fatal(err)
		}
	})
	got := phone.config(t)
	f := got.Folder("docs")
	if f == nil || f.Role != config.RoleRelay || f.Secret != "" || f.AwaitingSecret {
		t.Fatalf("relay folder wrong: %+v", f)
	}
	if got.Device(b.id.ID) == nil {
		t.Fatal("the relay did not learn about the full device B")
	}
	waitConfigHas(t, a, phone)
	a.reload(t)
	eventually(t, "B learnt the phone is a relay", func() bool {
		c, err := app.LoadConfig(b.home)
		return err == nil && c.Folder("docs").IsRelay(phone.id.ID)
	})
}

// The scenario that motivated automatic introductions: a network of a hub A, a second
// computer B and a relay R1, all paired through A. A second relay R2 is paired with A
// alone and must end up connected to B (and B to it) without any further pairing. It
// has no use for the other relay, so it is not introduced to it.
func TestNewRelayPairedWithOneDeviceIsIntroducedToTheOthers(t *testing.T) {
	root := t.TempDir()
	a, b, r1 := newDev(t, root, "A", false), newDev(t, root, "B", false), newDev(t, root, "R1", true)
	a.start(t, b, r1)
	b.start(t, a)
	r1.start(t, a)
	r2, r2cfg := freshGuest(t, root, "R2")

	inv, err := app.CreateNetworkInvite(a.home, a.config(t), a.id.ID, a.a.Node.Addrs(), app.NetworkInviteOptions{AsRelay: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := joinNetwork(t, a, r2, r2cfg, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	m := app.LoadMembership(r2.home, r2.id, quietLog())
	app.WithConfigLock(func() {
		if err := app.JoinNetwork(m, r2cfg, inv, resp, app.JoinOptions{Dir: r2.dir}); err != nil {
			t.Fatal(err)
		}
		if err := app.SaveConfig(r2.home, r2cfg); err != nil {
			t.Fatal(err)
		}
	})
	got := r2.config(t)
	if d := got.Device(b.id.ID); d == nil || got.Folder("docs").IsRelay(b.id.ID) {
		t.Fatalf("the new relay was not introduced to the full computer: %+v", d)
	}
	if got.Device(r1.id.ID) != nil {
		t.Fatal("the new relay was introduced to the other relay, which it never talks to")
	}

	waitConfigHas(t, a, r2)
	a.reload(t) // the hub tells the others
	eventually(t, "B learnt the new relay", func() bool {
		c, err := app.LoadConfig(b.home)
		return err == nil && c.Folder("docs").IsRelay(r2.id.ID)
	})
	b.reload(t)
	r2.reload(t)
	eventually(t, "B and the new relay are connected directly", b.connectedTo(r2))
}

// Two relays on one network: each buffers the change independently, the computer that
// comes back downloads it once, and both relays then forget it. A relay that vanished
// does not hold the other one back.
func TestTwoRelaysBothDeliverAndBothForget(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r1, r2 := newDev(t, root, "R1", true), newDev(t, root, "R2", true)
	a.start(t, b, r1, r2)
	r1.start(t, a, b)
	r2.start(t, a, b)

	big := randBytes(300 << 10)
	a.write(t, "doc.bin", big)
	eventually(t, "both relays hold the change", func() bool { return relayHolds(r1, 1) && relayHolds(r2, 1) })
	a.stop()

	b.start(t, a, r1, r2)
	eventually(t, "B received the file", b.hasContent("doc.bin", big))
	eventually(t, "both relays forgot it", func() bool { return storeEmpty(r1) && storeEmpty(r2) })

	// Now one relay disappears for good: the other one still delivers and forgets.
	b.stop()
	r1.stop()
	a.start(t, b, r1, r2)
	a.write(t, "later.txt", []byte("while B is away and R1 is gone"))
	eventually(t, "the remaining relay holds it", func() bool { return relayHolds(r2, 1) })
	a.stop()
	b.start(t, a, r1, r2)
	eventually(t, "B gets it from the remaining relay", b.hasContent("later.txt", []byte("while B is away and R1 is gone")))
	eventually(t, "the remaining relay forgets it", func() bool { return storeEmpty(r2) })
}
