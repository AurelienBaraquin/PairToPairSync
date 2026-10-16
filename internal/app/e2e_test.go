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

type dev struct {
	name  string
	home  string
	id    *identity.Identity
	port  int
	dir   string // synced directory (full) or storage directory (relay)
	relay bool
	quota int64 // relay quota in MB (0 = default)
	a     *app.App
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
	cfg := &config.Config{DeviceName: d.name, ListenAddr: fmt.Sprintf("127.0.0.1:%d", d.port), APIAddr: "-"}
	fc := config.Folder{ID: "docs", Path: d.dir, Role: config.RoleFull, ScanIntervalSec: 1}
	if d.relay {
		fc.Role = config.RoleRelay
		fc.QuotaMB = d.quota
	}
	for _, p := range peers {
		cfg.Devices = append(cfg.Devices, config.Device{ID: p.id.ID, Name: p.name, Addresses: []string{fmt.Sprintf("127.0.0.1:%d", p.port)}})
		fc.Devices = append(fc.Devices, config.FolderDevice{ID: p.id.ID, Relay: p.relay})
	}
	cfg.Folders = []config.Folder{fc}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
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
