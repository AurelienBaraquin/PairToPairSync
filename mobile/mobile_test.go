package mobile

import (
	"context"
	"encoding/json"
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
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

const secret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// computer starts a "PC": a full device with a folder, as the daemon would.
type computer struct {
	home string
	id   *identity.Identity
	a    *app.App
	port int
}

func newComputer(t *testing.T) *computer {
	t.Helper()
	c := &computer{home: t.TempDir(), port: freePort(t)}
	id, err := identity.LoadOrCreate(c.home)
	if err != nil {
		t.Fatal(err)
	}
	c.id = id
	cfg := config.Default()
	cfg.DeviceName, cfg.ListenPort, cfg.APIAddr = "pc", c.port, "-"
	cfg.LANDiscover, cfg.NATTraversal, cfg.DHT, cfg.RelayService = false, false, false, false
	cfg.ListenAddresses = []string{fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", c.port), fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", c.port)}
	cfg.Folders = []config.Folder{{ID: "docs", Label: "Documents", Path: filepath.Join(c.home, "docs"), Role: config.RoleFull, Secret: secret}}
	if err := app.SaveConfig(c.home, cfg); err != nil {
		t.Fatal(err)
	}
	c.start(t)
	t.Cleanup(func() { c.a.Stop() })
	return c
}

func (c *computer) start(t *testing.T) {
	t.Helper()
	cfg, err := app.LoadConfig(c.home)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Start(context.Background(), c.home, cfg, c.id, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.a = a
}

func (c *computer) invite(t *testing.T, asRelay bool, ttl time.Duration) string {
	t.Helper()
	cfg, _ := app.LoadConfig(c.home)
	inv, err := app.CreateInvite(c.home, cfg, c.id.ID, c.a.Node.Addrs(), "docs", asRelay, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Encode()
}

func newPhone(t *testing.T) (home, storage string) {
	t.Helper()
	home = t.TempDir()
	if _, err := Init(home, "Pixel"); err != nil {
		t.Fatal(err)
	}
	must(t, SetListenPort(home, freePort(t)))
	must(t, SetLANDiscovery(home, false))
	t.Cleanup(Stop)
	return home, filepath.Join(home, "relay")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

func TestInitIsIdempotentAndStable(t *testing.T) {
	home := t.TempDir()
	a, err := Init(home, "My <b>Phone</b>")
	must(t, err)
	b, err := Init(home, "Other name")
	must(t, err)
	if a != b || a != DeviceID(home) || !strings.HasPrefix(a, "12D3Koo") {
		t.Fatalf("id not stable: %q %q %q", a, b, DeviceID(home))
	}
	cj, _ := ConfigJSON(home)
	if strings.ContainsAny(cj, "<>") {
		t.Fatalf("markup in device name: %s", cj)
	}
}

func TestPhonePairsAsRelayFromAScannedCode(t *testing.T) {
	pc := newComputer(t)
	home, storage := newPhone(t)

	relayInvite := pc.invite(t, true, 0)
	summary, err := Pair(home, relayInvite, storage, 512)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]string
	_ = json.Unmarshal([]byte(summary), &s)
	if s["folder"] != "docs" || s["peer"] != "pc" {
		t.Fatalf("summary: %v", s)
	}

	cj, _ := ConfigJSON(home)
	if strings.Contains(cj, secret) || strings.Contains(strings.ToLower(cj), `"secret"`) {
		t.Fatalf("the phone's configuration contains the folder secret: %s", cj)
	}
	var view struct {
		Folders []struct {
			ID, Role string
			QuotaMB  int64 `json:"quota_mb"`
		}
	}
	_ = json.Unmarshal([]byte(cj), &view)
	if len(view.Folders) != 1 || view.Folders[0].Role != "relay" || view.Folders[0].QuotaMB != 512 {
		t.Fatalf("folder view: %s", cj)
	}
	if fi, err := os.Stat(filepath.Join(storage, "docs")); err != nil || !fi.IsDir() {
		t.Fatal("storage directory not created")
	}

	// The computer records the phone (after its 500 ms commit delay), then reloads.
	eventually(t, "computer recorded the phone", func() bool {
		cfg, err := app.LoadConfig(pc.home)
		return err == nil && cfg.Folder("docs").IsRelay(identity.Format(DeviceIDRaw(home)))
	})
	pc.a.Stop()
	pc.start(t)

	must(t, Start(home))
	if !IsRunning() {
		t.Fatal("not running")
	}
	eventually(t, "phone and computer connect", func() bool {
		var st struct {
			Running bool
			Status  struct{ Peers []struct{ Name string } }
		}
		_ = json.Unmarshal([]byte(StatusJSON()), &st)
		return st.Running && len(st.Status.Peers) == 1
	})
	if Logs() == "" {
		t.Fatal("no diagnostics logged")
	}
	// The Android app's unit tests parse real engine output: regenerate the
	// fixtures with P2P_FIXTURES=<dir> go test ./mobile -run TestPhonePairs
	if dir := os.Getenv("P2P_FIXTURES"); dir != "" {
		cj, _ := ConfigJSON(home)
		for name, content := range map[string]string{"status.json": StatusJSON(), "config.json": cj, "invite.txt": relayInvite} {
			must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		}
	}

	// The phone can invite a computer too (relay shows the QR, computer scans it).
	code, err := NewInvite(home, "docs")
	must(t, err)
	inv, err := pairing.Decode(code)
	must(t, err)
	if !inv.InviterRelay || inv.InviteeRelay {
		t.Fatalf("a relay's invitation must be relay->full: %+v", inv)
	}
	Stop()
	if IsRunning() {
		t.Fatal("still running after Stop")
	}
	if _, err := NewInvite(home, "docs"); err == nil {
		t.Fatal("invitation created while stopped")
	}
}

// DeviceIDRaw is the canonical ID (test helper).
func DeviceIDRaw(home string) string { return DeviceID(home) }

func TestPhoneRefusesWhatItCannotDo(t *testing.T) {
	pc := newComputer(t)
	home, storage := newPhone(t)

	if _, err := Pair(home, pc.invite(t, false, 0), storage, 0); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("a full-copy invitation must be refused with a helpful message, got %v", err)
	}
	if _, err := Pair(home, "not a code", storage, 0); err == nil {
		t.Fatal("garbage accepted")
	}
	short := pc.invite(t, true, time.Second)
	time.Sleep(2200 * time.Millisecond)
	if _, err := Pair(home, short, storage, 0); err == nil {
		t.Fatal("expired invitation accepted")
	}
	cj, _ := ConfigJSON(home)
	if strings.Contains(cj, `"id":"docs"`) {
		t.Fatal("a refused invitation left a folder behind")
	}
}

func TestEditsAndRemovals(t *testing.T) {
	pc := newComputer(t)
	home, storage := newPhone(t)
	_, err := Pair(home, pc.invite(t, true, 0), storage, 0)
	must(t, err)

	must(t, SetQuota(home, "docs", 1024))
	must(t, SetRetentionHours(home, "docs", 48))
	must(t, SetDeviceName(home, "Mon <téléphone>"))
	cj, _ := ConfigJSON(home)
	var v struct {
		DeviceName string `json:"device_name"`
		Folders    []struct {
			QuotaMB        int64 `json:"quota_mb"`
			RetentionHours int   `json:"retention_hours"`
		}
		Devices []struct{ ID string }
	}
	_ = json.Unmarshal([]byte(cj), &v)
	if v.Folders[0].QuotaMB != 1024 || v.Folders[0].RetentionHours != 48 || strings.ContainsAny(v.DeviceName, "<>") {
		t.Fatalf("edits not applied: %s", cj)
	}
	if err := SetQuota(home, "nope", 1); err == nil {
		t.Fatal("unknown folder accepted")
	}

	must(t, RemoveDevice(home, v.Devices[0].ID))
	cj, _ = ConfigJSON(home)
	if strings.Contains(cj, v.Devices[0].ID) {
		t.Fatal("device not removed")
	}
	must(t, RemoveFolder(home, "docs"))
	if _, err := os.Stat(filepath.Join(storage, "docs")); !os.IsNotExist(err) {
		t.Fatal("stored data not deleted with the folder")
	}
}

// Changing the configuration while running (what pairing does) restarts the engine.
func TestSupervisorReloadsOnConfigChange(t *testing.T) {
	home, _ := newPhone(t)
	must(t, Start(home))
	first := currentApp()
	if first == nil {
		t.Fatal("no app")
	}
	must(t, SetDeviceName(home, "Renamed"))
	eventually(t, "engine restarted after the config changed", func() bool {
		a := currentApp()
		return a != nil && a != first
	})
	Restart()
	cur := currentApp()
	eventually(t, "explicit restart", func() bool { a := currentApp(); return a != nil && a != cur })
	if err := Start(home); err == nil {
		t.Fatal("second Start accepted")
	}
}

func currentApp() *app.App {
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil {
		return nil
	}
	return r.app()
}

func TestRing(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(r, "line %d\n", i)
	}
	if got := r.String(); got != "line 2\nline 3\nline 4" {
		t.Fatalf("ring: %q", got)
	}
}
