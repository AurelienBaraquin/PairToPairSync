package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/folder"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
	"github.com/aurelienbaraquin/pairtopairsync/internal/relay"
)

func init() {
	folder.WatchDebounce = 50 * time.Millisecond
	folder.PullRetryMax = time.Second
	relay.HoldTime = 300 * time.Millisecond
	relay.ProbeInterval = time.Second
	relay.TickInterval = 100 * time.Millisecond
	app.RedialMax = 500 * time.Millisecond
}

const testSecret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

type dev struct {
	name  string
	home  string
	id    *identity.Identity
	port  int
	dir   string // synced directory (full) or storage directory (relay)
	relay bool
	quota int64 // relay quota in MB (0 = default)

	// connectivity features (all off unless a test turns them on)
	nat, relaySvc, dht, lan bool
	addr                    map[string]string // peer name -> address to use instead of 127.0.0.1:port
	a                       *app.App
}

func newDev(t *testing.T, root, name string, isRelay bool) *dev {
	t.Helper()
	d := &dev{name: name, home: filepath.Join(root, name, "home"), relay: isRelay}
	d.dir = filepath.Join(root, name, "data")
	id, err := identity.LoadOrCreate(d.home)
	if err != nil {
		t.Fatal(err)
	}
	d.id = id
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	return d
}

// start launches d knowing the given peers.
func (d *dev) start(t *testing.T, peers ...*dev) {
	t.Helper()
	cfg := &config.Config{DeviceName: d.name, ListenPort: d.port, APIAddr: "-",
		ListenAddresses: []string{
			fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", d.port),
			fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", d.port),
		}}
	fc := config.Folder{ID: "docs", Path: d.dir, Role: config.RoleFull, ScanIntervalSec: 1, Secret: testSecret}
	if d.relay {
		fc.Role = config.RoleRelay
		fc.Secret = "" // a relay never holds the folder secret
		fc.QuotaMB = d.quota
	}
	cfg.NATTraversal, cfg.RelayService, cfg.DHT, cfg.LANDiscover = d.nat, d.relaySvc, d.dht, d.lan
	for _, p := range peers {
		address := fmt.Sprintf("127.0.0.1:%d", p.port)
		if a, ok := d.addr[p.name]; ok {
			address = a
		}
		var addrs []string
		if address != "" {
			addrs = []string{address}
		}
		cfg.Devices = append(cfg.Devices, config.Device{ID: p.id.ID, Name: p.name, Addresses: addrs})
		fc.Devices = append(fc.Devices, config.FolderDevice{ID: p.id.ID, Relay: p.relay})
	}
	cfg.Folders = []config.Folder{fc}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(filepath.Join(d.home, app.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	d.launch(t, cfg)
}

func (d *dev) launch(t *testing.T, cfg *config.Config) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("P2P_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("dev", d.name)
	}
	a, err := app.Start(context.Background(), d.home, cfg, d.id, log)
	if err != nil {
		t.Fatal(err)
	}
	d.a = a
	t.Cleanup(d.stop)
}

func (d *dev) stop() {
	if d.a != nil {
		d.a.Stop()
		d.a = nil
	}
}

func (d *dev) path(rel string) string { return filepath.Join(d.dir, filepath.FromSlash(rel)) }

func (d *dev) write(t *testing.T, rel string, data []byte) {
	t.Helper()
	p := d.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

func (d *dev) hasContent(rel string, want []byte) func() bool {
	return func() bool {
		b, err := os.ReadFile(d.path(rel))
		return err == nil && bytes.Equal(b, want)
	}
}

func (d *dev) gone(rel string) func() bool {
	return func() bool { _, err := os.Lstat(d.path(rel)); return os.IsNotExist(err) }
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// storeEmpty reports whether a relay holds no pending change and no data block.
func storeEmpty(d *dev) bool {
	b, err := os.ReadFile(filepath.Join(d.dir, "pending.json"))
	if err != nil || strings.TrimSpace(string(b)) != "{}" {
		return false
	}
	empty := true
	_ = filepath.WalkDir(filepath.Join(d.dir, "blocks"), func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			empty = false
		}
		return nil
	})
	return empty
}

// storeHasBlocks reports whether a relay completely holds at least one pending
// change (all of its data blocks are stored).
func storeHasBlocks(d *dev) bool {
	b, err := os.ReadFile(filepath.Join(d.dir, "pending.json"))
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s != "" && s != "{}" && !strings.Contains(s, `"have":false`)
}

func TestDirectSync(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	big := randBytes(1<<20 + 12345) // several blocks
	a.write(t, "hello.txt", []byte("hello"))
	a.write(t, "sub/dir/big.bin", big)
	a.start(t, b)
	b.start(t, a)

	eventually(t, "initial files reach B", func() bool {
		return b.hasContent("hello.txt", []byte("hello"))() && b.hasContent("sub/dir/big.bin", big)()
	})

	// Live edits, both ways.
	a.write(t, "hello.txt", []byte("hello world"))
	eventually(t, "edit reaches B", b.hasContent("hello.txt", []byte("hello world")))
	b.write(t, "from-b.txt", []byte("hi A"))
	eventually(t, "new file reaches A", a.hasContent("from-b.txt", []byte("hi A")))

	// Delta: change the first bytes of a large file; result must still be exact.
	big2 := append([]byte{}, big...)
	copy(big2, []byte("PATCHED!"))
	a.write(t, "sub/dir/big.bin", big2)
	eventually(t, "patched big file reaches B", b.hasContent("sub/dir/big.bin", big2))

	// Deletions propagate, including whole directories.
	if err := os.Remove(a.path("hello.txt")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deletion reaches B", b.gone("hello.txt"))
	if err := os.RemoveAll(a.path("sub")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "directory deletion reaches B", b.gone("sub"))
}

func TestConflictKeepsBothVersions(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	a.write(t, "doc.txt", []byte("version from A"))
	time.Sleep(1100 * time.Millisecond)
	b.write(t, "doc.txt", []byte("version from B")) // newer: wins
	a.start(t, b)
	b.start(t, a)

	eventually(t, "winner on both sides", func() bool {
		return a.hasContent("doc.txt", []byte("version from B"))() && b.hasContent("doc.txt", []byte("version from B"))()
	})
	hasLoser := func(d *dev) bool {
		m, _ := filepath.Glob(filepath.Join(d.dir, "doc.sync-conflict-*.txt"))
		if len(m) != 1 {
			return false
		}
		c, _ := os.ReadFile(m[0])
		return string(c) == "version from A"
	}
	eventually(t, "loser preserved as conflict copy on both", func() bool { return hasLoser(a) && hasLoser(b) })
}

func TestIdenticalFoldersDoNotConflict(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	a.write(t, "same.txt", []byte("identical"))
	b.write(t, "same.txt", []byte("identical"))
	a.start(t, b)
	b.start(t, a)
	a.write(t, "marker.txt", []byte("m"))
	eventually(t, "sync settled", b.hasContent("marker.txt", []byte("m")))
	for _, d := range []*dev{a, b} {
		if m, _ := filepath.Glob(filepath.Join(d.dir, "*conflict*")); len(m) != 0 {
			t.Fatalf("%s: unexpected conflict copies: %v", d.name, m)
		}
	}
}

// The headline scenario: A and B are never online at the same time.
func TestRelayDeliversToAbsentDeviceThenForgets(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r := newDev(t, root, "R", true)

	a.start(t, b, r)
	r.start(t, a, b)

	big := randBytes(700 << 10)
	a.write(t, "notes/todo.txt", []byte("buy milk"))
	a.write(t, "photo.bin", big)

	eventually(t, "relay stored the changes", func() bool { return storeHasBlocks(r) })
	a.stop() // A goes away before B ever existed on the network

	b.start(t, a, r)
	eventually(t, "B received files through the relay", func() bool {
		return b.hasContent("notes/todo.txt", []byte("buy milk"))() && b.hasContent("photo.bin", big)()
	})
	eventually(t, "relay forgot delivered data", func() bool { return storeEmpty(r) })

	// Deletion made while the other side is away also travels through the relay.
	b.stop()
	a.start(t, b, r)
	a.write(t, "later.txt", []byte("sent while B is away"))
	if err := os.Remove(a.path("notes/todo.txt")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "relay stores new change", func() bool { return storeHasBlocks(r) })
	a.stop()
	b.start(t, a, r)
	eventually(t, "B got new file and deletion", func() bool {
		return b.hasContent("later.txt", []byte("sent while B is away"))() && b.gone("notes/todo.txt")()
	})
	eventually(t, "relay empty again", func() bool { return storeEmpty(r) })
}

// When both full devices are online they sync directly; the relay must not
// copy the data (it is only a safety net) and must still end up empty.
func TestRelayStaysEmptyWhenEveryoneIsOnline(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r := newDev(t, root, "R", true)
	a.start(t, b, r)
	b.start(t, a, r)
	r.start(t, a, b)
	time.Sleep(500 * time.Millisecond)

	data := randBytes(300 << 10)
	a.write(t, "live.bin", data)
	eventually(t, "B got file directly", b.hasContent("live.bin", data))
	eventually(t, "relay empty", func() bool { return storeEmpty(r) })
}

func TestIgnoredFilesAreNotSynced(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	a.write(t, ".p2pignore", []byte("*.tmp\nbuild/\n"))
	a.write(t, "keep.txt", []byte("k"))
	a.write(t, "junk.tmp", []byte("x"))
	a.write(t, "build/out.o", []byte("x"))
	a.start(t, b)
	b.start(t, a)
	eventually(t, "kept file synced", b.hasContent("keep.txt", []byte("k")))
	time.Sleep(500 * time.Millisecond)
	for _, p := range []string{"junk.tmp", "build"} {
		if _, err := os.Stat(b.path(p)); err == nil {
			t.Fatalf("%s should not have been synced", p)
		}
	}
}

// When the relay's quota is too small it simply does not store the change; the
// devices still converge by syncing directly and the relay cleans up afterwards.
func TestRelayQuotaIsRespected(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r := newDev(t, root, "R", true)
	r.quota = 1 // MB
	a.start(t, b, r)
	r.start(t, a, b)

	big := randBytes(2 << 20)
	a.write(t, "big.bin", big)
	time.Sleep(2 * time.Second)
	if n := countBlocks(r); n != 0 {
		t.Fatalf("relay stored %d blocks despite its 1 MB quota", n)
	}

	b.start(t, a, r)
	eventually(t, "B gets the file directly from A", b.hasContent("big.bin", big))
	eventually(t, "relay cleaned up after B confirmed", func() bool { return storeEmpty(r) })
}

func countBlocks(d *dev) int {
	n := 0
	_ = filepath.WalkDir(filepath.Join(d.dir, "blocks"), func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// A device that is not in the configuration must not be able to read anything,
// even though it speaks the protocol and has a valid certificate.
func TestUnknownDeviceIsRefused(t *testing.T) {
	root := t.TempDir()
	a, intruder := newDev(t, root, "A", false), newDev(t, root, "X", false)
	a.write(t, "secret.txt", []byte("top secret"))
	a.start(t) // knows nobody
	intruder.start(t, a)
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(intruder.path("secret.txt")); err == nil {
		t.Fatal("an unknown device received data")
	}
	if peers := a.a.Node.Peers(); len(peers) != 0 {
		t.Fatalf("unknown device was accepted: %v", peers)
	}
}

// The relay is blind: after storing real files, nothing on its disk contains a
// file name or any fragment of the content.
func TestRelayNeverSeesPlaintext(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r := newDev(t, root, "R", true)
	a.start(t, b, r)
	r.start(t, a, b)

	secretText := []byte("TOP-SECRET-CONTENT-" + strings.Repeat("x", 5000))
	a.write(t, "Confidential/salary-review-2026.txt", secretText)
	eventually(t, "relay stored the change", func() bool { return storeHasBlocks(r) })

	var checked int
	_ = filepath.WalkDir(r.dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(p)
		checked++
		for _, leak := range []string{"TOP-SECRET", "salary-review", "Confidential", "xxxxxxxx"} {
			if bytes.Contains(raw, []byte(leak)) || strings.Contains(p, leak) {
				t.Errorf("relay leaks %q in %s", leak, p)
			}
		}
		return nil
	})
	if checked < 2 {
		t.Fatalf("expected relay files to inspect, got %d", checked)
	}

	// And the data still arrives intact on a device that gets it only via the relay.
	a.stop()
	b.start(t, a, r)
	eventually(t, "B decrypts and receives", b.hasContent("Confidential/salary-review-2026.txt", secretText))
}

// A relay that tampers with a block must not be able to corrupt a device.
func TestTamperedRelayBlockIsRejected(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	r := newDev(t, root, "R", true)
	a.start(t, b, r)
	r.start(t, a, b)
	a.write(t, "doc.txt", []byte(strings.Repeat("original ", 2000)))
	eventually(t, "relay stored the change", func() bool { return storeHasBlocks(r) })
	a.stop()

	// Corrupt every stored block.
	_ = filepath.WalkDir(filepath.Join(r.dir, "blocks"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			raw, _ := os.ReadFile(p)
			raw[len(raw)/2] ^= 0xff
			_ = os.WriteFile(p, raw, 0o600)
		}
		return nil
	})
	b.start(t, a, r)
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(b.path("doc.txt")); err == nil {
		t.Fatal("device accepted tampered data from the relay")
	}
}

func (d *dev) status() (app.Status, folder.Status) {
	st := d.a.Status().(app.Status)
	var fs folder.Status
	for _, f := range st.Folders {
		if v, ok := f.(folder.Status); ok {
			fs = v
		}
	}
	return st, fs
}

func (d *dev) connectedTo(other *dev) func() bool {
	return func() bool {
		if d.a == nil {
			return false
		}
		for _, p := range d.a.Node.Peers() {
			if p.ID == other.id.ID {
				return true
			}
		}
		return false
	}
}

// A big download interrupted by the sender going away resumes where it stopped
// instead of starting over (important on unstable links).
func TestInterruptedDownloadResumes(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	big := randBytes(12 << 20) // 96 blocks
	a.write(t, "big.bin", big)

	folder.SetServeDelay(40 * time.Millisecond)
	t.Cleanup(func() { folder.SetServeDelay(0) })
	a.start(t, b)
	b.start(t, a)

	partial := func() int64 {
		var best int64
		m, _ := filepath.Glob(filepath.Join(b.dir, ".p2psync", "tmp", "*.part"))
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.Size() > best {
				best = fi.Size()
			}
		}
		return best
	}
	eventually(t, "download in progress", func() bool { return partial() > 4<<20 })
	a.stop() // the link dies mid-transfer
	if b.hasContent("big.bin", big)() {
		t.Fatal("transfer finished before the interruption; test is not exercising resume")
	}

	folder.SetServeDelay(0)
	a.start(t, b)
	eventually(t, "download completes after reconnection", b.hasContent("big.bin", big))
	_, fs := a.status()
	if fs.SentBytes > int64(len(big))*8/10 {
		t.Fatalf("after reconnecting A re-sent %d of %d bytes: the partial download was not resumed", fs.SentBytes, len(big))
	}
	t.Logf("A re-sent only %.1f MB of %.1f MB", float64(fs.SentBytes)/1e6, float64(len(big))/1e6)
}

func privateHook(public string) func(*node.Options) {
	return func(o *node.Options) {
		o.Reachability = "private"
		if o.Config.DeviceName == public {
			o.Reachability = "public"
		}
	}
}

// Two devices that cannot reach each other (no address known, as behind two
// NATs) still connect, through a third device of the same owner acting as a
// circuit relay. No external server is involved.
func TestDevicesConnectThroughOwnDeviceAsCircuitRelay(t *testing.T) {
	app.NodeHook = privateHook("H")
	t.Cleanup(func() { app.NodeHook = nil })

	root := t.TempDir()
	h, a, b := newDev(t, root, "H", false), newDev(t, root, "A", false), newDev(t, root, "B", false)
	h.relaySvc, h.nat = true, true
	a.nat, b.nat = true, true
	via := fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1/p2p/%s/p2p-circuit", h.port, h.id.ID)
	a.addr = map[string]string{"B": via}
	b.addr = map[string]string{"A": via}

	h.start(t, a, b)
	a.start(t, h, b)
	b.start(t, h, a)

	eventually(t, "A and B talk to each other through H", func() bool { return a.connectedTo(b)() && b.connectedTo(a)() })

	data := randBytes(500 << 10)
	a.write(t, "via-relay.bin", data)
	eventually(t, "file reaches B", b.hasContent("via-relay.bin", data))
}

// Devices only need to know *one* reachable device of their own: the private
// DHT lets them learn each other's current addresses from it.
func TestDevicesFindEachOtherThroughPrivateDHT(t *testing.T) {
	app.NodeHook = privateHook("H")
	t.Cleanup(func() { app.NodeHook = nil })

	root := t.TempDir()
	h, a, b := newDev(t, root, "H", false), newDev(t, root, "A", false), newDev(t, root, "B", false)
	for _, d := range []*dev{h, a, b} {
		d.nat, d.relaySvc, d.dht = true, true, true
	}
	a.addr = map[string]string{"B": ""}
	b.addr = map[string]string{"A": ""}

	h.start(t, a, b)
	a.start(t, h, b)
	b.start(t, h, a)

	eventually(t, "A and B find each other", func() bool { return a.connectedTo(b)() && b.connectedTo(a)() })
}

// A daemon must always be able to stop, even if a networking component hangs on
// close (this happened with LAN discovery in a multicast-less environment).
func TestStopIsBounded(t *testing.T) {
	root := t.TempDir()
	a, b := newDev(t, root, "A", false), newDev(t, root, "B", false)
	a.nat, a.dht, a.lan, a.relaySvc = true, true, true, true
	a.start(t, b)
	time.Sleep(500 * time.Millisecond)
	done := make(chan struct{})
	go func() { a.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// ---- QR pairing --------------------------------------------------------------------

// reload restarts a device from the configuration on disk, as the daemon does
// when it notices config.json changed.
func (d *dev) reload(t *testing.T) {
	t.Helper()
	d.stop()
	cfg, err := app.LoadConfig(d.home)
	if err != nil {
		t.Fatal(err)
	}
	d.launch(t, cfg)
}

// freshGuest is a brand-new device with no configuration besides its own ports.
func freshGuest(t *testing.T, root, name string) (*dev, *config.Config) {
	t.Helper()
	g := newDev(t, root, name, false)
	cfg := config.Default()
	cfg.DeviceName, cfg.ListenPort, cfg.APIAddr = name, g.port, "-"
	cfg.LANDiscover, cfg.NATTraversal, cfg.DHT, cfg.RelayService = false, false, false, false
	cfg.ListenAddresses = []string{fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", g.port), fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", g.port)}
	return g, cfg
}

func (d *dev) invite(t *testing.T, asRelay bool, ttl time.Duration) pairing.Invite {
	t.Helper()
	cfg, err := app.LoadConfig(d.home)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := app.CreateInvite(d.home, cfg, d.id.ID, d.a.Node.Addrs(), "docs", asRelay, ttl)
	if err != nil {
		t.Fatal(err)
	}
	// What the QR code carries must survive being printed and scanned.
	back, err := pairing.Decode(inv.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func scan(g *dev, cfg *config.Config, inv pairing.Invite) (protocol.PairResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return pairing.Dial(ctx, g.id.Key, inv, app.GuestRequest(cfg))
}

func waitConfigHas(t *testing.T, d *dev, other *dev) {
	t.Helper()
	eventually(t, d.name+" recorded the new device", func() bool {
		cfg, err := app.LoadConfig(d.home)
		return err == nil && cfg.Device(other.id.ID) != nil
	})
}

// One scan pairs two computers: both trust each other, the guest receives the
// folder secret, and files start flowing.
func TestQRPairFullDevices(t *testing.T) {
	root := t.TempDir()
	a := newDev(t, root, "A", false)
	a.start(t) // A knows nobody yet
	g, gcfg := freshGuest(t, root, "G")

	inv := a.invite(t, false, 0)
	resp, err := scan(g, gcfg, inv)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Secret != testSecret || resp.Folder != "docs" {
		t.Fatalf("guest did not receive the folder: %+v", resp)
	}
	if err := app.PreflightJoin(gcfg, inv, app.JoinOptions{Path: g.dir}); err != nil {
		t.Fatal(err)
	}
	if err := app.Join(gcfg, inv, resp, app.JoinOptions{Path: g.dir}); err != nil {
		t.Fatal(err)
	}
	if err := app.SaveConfig(g.home, gcfg); err != nil {
		t.Fatal(err)
	}
	waitConfigHas(t, a, g)
	a.reload(t)
	g.reload(t)

	a.write(t, "after-pairing.txt", []byte("hello guest"))
	eventually(t, "file flows after QR pairing", g.hasContent("after-pairing.txt", []byte("hello guest")))
	g.write(t, "reply.txt", []byte("hello A"))
	eventually(t, "and back", a.hasContent("reply.txt", []byte("hello A")))
}

// A phone invited "as relay" never receives the folder secret, and the relay
// workflow works straight after pairing.
func TestQRPairRelayNeverGetsSecret(t *testing.T) {
	root := t.TempDir()
	a := newDev(t, root, "A", false)
	a.start(t)
	phone, pcfg := freshGuest(t, root, "Phone")

	inv := a.invite(t, true, 0)
	if !inv.InviteeRelay {
		t.Fatal("invitation lost the relay role")
	}
	resp, err := scan(phone, pcfg, inv)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Secret != "" {
		t.Fatal("the folder secret was sent to a relay")
	}
	opts := app.JoinOptions{Path: phone.dir}
	if err := app.Join(pcfg, inv, resp, opts); err != nil {
		t.Fatal(err)
	}
	if err := app.SaveConfig(phone.home, pcfg); err != nil {
		t.Fatal(err)
	}
	if f := pcfg.Folder("docs"); f.Role != config.RoleRelay || f.Secret != "" {
		t.Fatalf("relay folder misconfigured: %+v", f)
	}
	waitConfigHas(t, a, phone)
	acfg, _ := app.LoadConfig(a.home)
	if !acfg.Folder("docs").IsRelay(phone.id.ID) {
		t.Fatal("inviter did not register the guest as a relay")
	}
	phone.relay = true
	a.reload(t)
	phone.reload(t)

	// A second computer joins through two more scans: A's QR (full copy), then the
	// phone's QR (the phone, as inviter, becomes B's relay). B then stays offline.
	b, bcfg := freshGuest(t, root, "B")
	invA := a.invite(t, false, 0)
	respA, err := scan(b, bcfg, invA)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Join(bcfg, invA, respA, app.JoinOptions{Path: b.dir}); err != nil {
		t.Fatal(err)
	}
	invP := phone.invite(t, false, 0)
	if err := app.PreflightJoin(bcfg, invP, app.JoinOptions{}); err != nil {
		t.Fatal(err)
	}
	respP, err := scan(b, bcfg, invP)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Join(bcfg, invP, respP, app.JoinOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := app.SaveConfig(b.home, bcfg); err != nil {
		t.Fatal(err)
	}
	waitConfigHas(t, a, b)
	waitConfigHas(t, phone, b)
	a.reload(t)
	phone.reload(t)

	a.write(t, "via-phone.txt", []byte("stored on the phone"))
	eventually(t, "relay holds the sealed change for absent B", func() bool { return storeHasBlocks(phone) })

	// B finally comes online and gets the file through the phone alone.
	a.stop()
	b.reload(t)
	eventually(t, "B receives it from the relay", b.hasContent("via-phone.txt", []byte("stored on the phone")))
	eventually(t, "phone forgets it", func() bool { return storeEmpty(phone) })
}

// A relay can invite a full device too (the phone shows the QR, the PC scans).
func TestQRInviteFromRelay(t *testing.T) {
	root := t.TempDir()
	relayDev := newDev(t, root, "R", true)
	relayDev.start(t)
	g, gcfg := freshGuest(t, root, "G")
	// The guest already has the folder (and its secret) from elsewhere.
	gcfg.Folders = []config.Folder{{ID: "docs", Path: g.dir, Role: config.RoleFull, Secret: testSecret}}

	inv := relayDev.invite(t, false, 0)
	if !inv.InviterRelay {
		t.Fatal("invitation should say the inviter is a relay")
	}
	if err := app.PreflightJoin(gcfg, inv, app.JoinOptions{}); err != nil {
		t.Fatal(err)
	}
	resp, err := scan(g, gcfg, inv)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Secret != "" {
		t.Fatal("a relay has no secret to hand out")
	}
	if err := app.Join(gcfg, inv, resp, app.JoinOptions{}); err != nil {
		t.Fatal(err)
	}
	if !gcfg.Folder("docs").IsRelay(relayDev.id.ID) {
		t.Fatal("guest did not register the inviter as relay")
	}
	waitConfigHas(t, relayDev, g)
	rcfg, _ := app.LoadConfig(relayDev.home)
	if rcfg.Folder("docs").IsRelay(g.id.ID) || !rcfg.Folder("docs").SharesWith(g.id.ID) {
		t.Fatal("relay must list the guest as a full device")
	}
}

func TestQRRejectsReuseExpiryAndBadTokens(t *testing.T) {
	root := t.TempDir()
	a := newDev(t, root, "A", false)
	a.start(t)

	// Single use.
	g1, c1 := freshGuest(t, root, "G1")
	inv := a.invite(t, false, 0)
	if _, err := scan(g1, c1, inv); err != nil {
		t.Fatal(err)
	}
	g2, c2 := freshGuest(t, root, "G2")
	if _, err := scan(g2, c2, inv); err == nil {
		t.Fatal("the same invitation worked twice")
	}

	// Expiry.
	short := a.invite(t, false, time.Second)
	time.Sleep(2200 * time.Millisecond)
	g3, c3 := freshGuest(t, root, "G3")
	if _, err := scan(g3, c3, short); err == nil {
		t.Fatal("expired invitation accepted")
	}

	// Wrong token while another invitation is outstanding: refused, nothing leaked, nothing added.
	valid := a.invite(t, false, 0)
	forged := valid
	forged.Token = "AAAAAAAAAAAAAAAAAAAAAA"
	g4, c4 := freshGuest(t, root, "G4")
	resp, err := scan(g4, c4, forged)
	if err == nil || resp.Secret != "" {
		t.Fatalf("forged token accepted: %+v", resp)
	}
	acfg, _ := app.LoadConfig(a.home)
	if acfg.Device(g4.id.ID) != nil || acfg.Device(g3.id.ID) != nil {
		t.Fatal("a device with a bad token was added to the configuration")
	}

	// Guessing is punished: after repeated failures every invitation is cancelled.
	for i := 0; i < 6; i++ {
		gx, cx := freshGuest(t, root, fmt.Sprintf("X%d", i))
		_, _ = scan(gx, cx, forged)
	}
	g5, c5 := freshGuest(t, root, "G5")
	if _, err := scan(g5, c5, valid); err == nil {
		t.Fatal("outstanding invitation survived a brute-force attempt")
	}
}

// Without an outstanding invitation a stranger cannot even complete the handshake.
func TestQRNoInvitationNoEntry(t *testing.T) {
	root := t.TempDir()
	a := newDev(t, root, "A", false)
	a.start(t)
	inv := a.invite(t, false, 0)
	pairing.NewStore(a.home).Purge() // invitation withdrawn
	g, gc := freshGuest(t, root, "G")
	if _, err := scan(g, gc, inv); err == nil {
		t.Fatal("stranger paired without an outstanding invitation")
	}
}
