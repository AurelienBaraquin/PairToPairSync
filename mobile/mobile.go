// Package mobile is the API the Android application calls (through gomobile).
//
// It only uses types gomobile can bridge (string, int64, bool, error). The phone
// acts as a temporary relay: it stores undelivered changes in the app's private
// directory and forgets them once every full device has them. All state lives
// under the `home` directory the app passes in (its private files directory).
//
//	gomobile bind -target=android/arm64 -androidapi 26 \
//	    -ldflags="-checklinkname=0" -o p2psync.aar github.com/aurelienbaraquin/pairtopairsync/mobile
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

// guard turns a panic inside the engine into an error: the Go runtime lives in the
// app's process, so an unrecovered panic would kill the whole app.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("internal error: %v", r)
	}
}

// quiet is guard for functions that return no error.
func quiet() { _ = recover() }

// Version of the embedded engine.
func Version() string { return "0.4.0" }

// ---- identity ------------------------------------------------------------------------

// Init creates the identity and a default configuration on first use and returns
// the device ID. It is idempotent.
func Init(home, deviceName string) (res string, err error) {
	defer guard(&err)
	id, err := app.Init(home, config.Clean(deviceName), 0, "-") // "-": no local web page on a phone
	if err != nil {
		return "", err
	}
	return identity.Format(id), nil
}

// DeviceID returns this device's ID ("" if not initialised).
func DeviceID(home string) string {
	defer quiet()
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return ""
	}
	return identity.Format(id.ID)
}

// ---- pairing -------------------------------------------------------------------------

// Pair uses the text of a scanned invitation (p2psync://pair/...). The phone can
// only be a relay, so the invitation must have been created with "--as-relay"
// (or "as relay" in the page). storageDir is where undelivered changes are kept
// temporarily. It returns a small JSON summary for the UI.
func Pair(home, code, storageDir string, quotaMB int64) (res string, err error) {
	defer guard(&err)
	inv, err := pairing.Decode(code)
	if err != nil {
		return "", err
	}
	if inv.Expired(time.Now()) {
		return "", errors.New("this invitation has expired; ask for a new one")
	}
	if inv.Net {
		return pairNetwork(home, inv, storageDir)
	}
	if !inv.InviteeRelay {
		return "", errors.New("this invitation is for a full copy, which a phone cannot hold; " +
			"create it with `p2psync invite --as-relay` (or tick \"relay\" on the page)")
	}
	if _, err := Init(home, ""); err != nil {
		return "", err
	}
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return "", err
	}
	opts := app.JoinOptions{Path: filepath.Join(storageDir, inv.Folder), QuotaMB: quotaMB}
	if err := app.PreflightJoin(cfg, inv, opts); err != nil {
		return "", err
	}
	if err := app.CheckJoinPath(home, cfg, inv, opts); err != nil {
		return "", err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return "", err
	}
	ctx, cancel := pairContext(45 * time.Second)
	defer cancel()
	resp, err := pairing.Dial(ctx, id.Key, inv, app.GuestRequest(cfg))
	if err != nil {
		return "", err
	}
	// Nothing was changed on disk until the other device accepted us. Re-read the
	// configuration under the lock: it may have changed during the (slow) network exchange.
	var joinErr error
	app.WithConfigLock(func() {
		if cfg, joinErr = app.LoadConfig(home); joinErr != nil {
			return
		}
		if joinErr = app.Join(cfg, inv, resp, opts); joinErr != nil {
			return
		}
		joinErr = app.SaveConfig(home, cfg)
	})
	if joinErr != nil {
		return "", joinErr
	}
	b, _ := json.Marshal(map[string]string{"folder": inv.Folder, "label": inv.Label, "peer": resp.Name, "peer_id": inv.ID})
	return string(b), nil
}

// pairCtx lets the app give up on a pairing that is waiting for the other owner.
var (
	pairMu     sync.Mutex
	pairCancel context.CancelFunc
)

func pairContext(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	pairMu.Lock()
	pairCancel = cancel
	pairMu.Unlock()
	return ctx, cancel
}

// CancelPair abandons the pairing in progress (for instance while it waits for the
// other device's owner to accept). The invitation is spent either way.
func CancelPair() {
	defer quiet()
	pairMu.Lock()
	c := pairCancel
	pairMu.Unlock()
	if c != nil {
		c()
	}
}

// pairNetwork joins a network through the full device that made the code: this phone
// becomes a relay for the folders it chose, and learns the other devices from its
// introductions. Its owner may have to accept the request, which can take minutes.
func pairNetwork(home string, inv pairing.Invite, storageDir string) (string, error) {
	if !inv.InviteeRelay {
		return "", errors.New("this invitation would make the phone a full copy, which it cannot hold; " +
			"ask for an invitation \"as relay\"")
	}
	if _, err := Init(home, ""); err != nil {
		return "", err
	}
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return "", err
	}
	if err := app.PreflightNetwork(cfg, inv); err != nil {
		return "", err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return "", err
	}
	wait := 45 * time.Second
	if inv.Approve {
		wait += pairing.MaxApprovalWait
	}
	ctx, cancel := pairContext(wait)
	defer cancel()
	resp, err := pairing.Dial(ctx, id.Key, inv, app.GuestRequest(cfg))
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("pairing cancelled or timed out")
		}
		return "", err
	}
	m := app.LoadMembership(home, id, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	var joinErr error
	app.WithConfigLock(func() {
		if cfg, joinErr = app.LoadConfig(home); joinErr != nil {
			return
		}
		if joinErr = app.JoinNetwork(m, cfg, inv, resp, app.JoinOptions{Dir: storageDir}); joinErr != nil {
			return
		}
		joinErr = app.SaveConfig(home, cfg)
	})
	if joinErr != nil {
		return "", joinErr
	}
	b, _ := json.Marshal(map[string]any{"folder": inv.Folder, "folders": inv.Folders, "label": inv.Label,
		"peer": resp.Name, "peer_id": inv.ID, "network": true})
	return string(b), nil
}

// NewInvite creates a single-use invitation for a full device that should use
// this phone as its relay (the phone shows the QR, the computer scans it with
// `p2psync pair`). The engine must be running. It returns the invitation text.
func NewInvite(home, folderID string) (res string, err error) {
	defer guard(&err)
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil || r.app() == nil {
		return "", errors.New("start the relay first")
	}
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return "", err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return "", err
	}
	inv, err := app.CreateInvite(home, cfg, id.ID, r.app().Node.Addrs(), folderID, false, 0)
	if err != nil {
		return "", err
	}
	return inv.Encode(), nil
}

// ---- configuration views and edits -------------------------------------------------------

type deviceView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Managed bool   `json:"managed,omitempty"` // introduced by another device rather than paired directly
}

type folderView struct {
	ID             string       `json:"id"`
	Label          string       `json:"label"`
	Role           string       `json:"role"`
	QuotaMB        int64        `json:"quota_mb"`
	RetentionHours int          `json:"retention_hours"`
	Devices        []deviceView `json:"devices"`
	Managed        bool         `json:"managed,omitempty"` // introduced by another device
}

// ConfigJSON describes devices and folders for the UI. It never contains secrets.
func ConfigJSON(home string) (res string, err error) {
	defer guard(&err)
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return "", err
	}
	out := struct {
		DeviceName string       `json:"device_name"`
		ListenPort int          `json:"listen_port"`
		LAN        bool         `json:"lan_discovery"`
		Devices    []deviceView `json:"devices"`
		Folders    []folderView `json:"folders"`
	}{DeviceName: cfg.DeviceName, ListenPort: cfg.ListenPort, LAN: cfg.LANDiscover, Devices: []deviceView{}, Folders: []folderView{}}
	for _, d := range cfg.Devices {
		out.Devices = append(out.Devices, deviceView{d.ID, d.Name, d.Managed})
	}
	for _, f := range cfg.Folders {
		fv := folderView{ID: f.ID, Label: f.Label, Role: f.Role, QuotaMB: f.Quota() >> 20,
			RetentionHours: int(f.Retention().Hours()), Devices: []deviceView{}, Managed: f.Managed}
		for _, fd := range f.Devices {
			name := fd.ID
			if d := cfg.Device(fd.ID); d != nil && d.Name != "" {
				name = d.Name
			}
			fv.Devices = append(fv.Devices, deviceView{ID: fd.ID, Name: name})
		}
		out.Folders = append(out.Folders, fv)
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// membership opens the grants of this device (nil if the identity cannot be read).
func membership(home string) *app.Membership {
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return nil
	}
	return app.LoadMembership(home, id, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func edit(home string, fn func(*config.Config) error) (err error) {
	defer guard(&err)
	app.WithConfigLock(func() {
		var cfg *config.Config
		if cfg, err = app.LoadConfig(home); err != nil {
			return
		}
		if err = fn(cfg); err != nil {
			return
		}
		err = app.SaveConfig(home, cfg)
	})
	return err
}

// RemoveDevice forgets a device everywhere. Pending changes it had not yet
// received no longer hold anything back.
func RemoveDevice(home, deviceID string) error {
	id, err := identity.Normalize(deviceID)
	if err != nil {
		return err
	}
	m := membership(home)
	return edit(home, func(c *config.Config) error {
		if d := c.Device(id); d != nil && d.Managed && m != nil {
			m.Dismiss(c, id) // the same introduction does not bring it back, only a newer one
		}
		var devs []config.Device
		for _, d := range c.Devices {
			if d.ID != id {
				devs = append(devs, d)
			}
		}
		c.Devices = devs
		for i := range c.Folders {
			var fd []config.FolderDevice
			for _, d := range c.Folders[i].Devices {
				if d.ID != id {
					fd = append(fd, d)
				}
			}
			c.Folders[i].Devices = fd
		}
		return nil
	})
}

// RemoveFolder stops relaying a folder and deletes everything stored for it.
func RemoveFolder(home, folderID string) (err error) {
	defer guard(&err)
	var path string
	m := membership(home)
	err = edit(home, func(c *config.Config) error {
		var keep []config.Folder
		for _, f := range c.Folders {
			if f.ID == folderID {
				path = f.Path
				if f.Managed && m != nil {
					m.DismissFolder(c, folderID)
				}
				continue
			}
			keep = append(keep, f)
		}
		if path == "" {
			return fmt.Errorf("unknown folder %q", folderID)
		}
		c.Folders = keep
		return nil
	})
	if err != nil {
		return err
	}
	// The running engine still has the folder open and could write to it again:
	// restart it on the new configuration *before* deleting what it stored.
	mu.Lock()
	r := current
	mu.Unlock()
	if r != nil {
		r.restart()
	}
	if path != "" && filepath.Base(path) == folderID { // only ever delete our own storage directory
		_ = os.RemoveAll(path)
	}
	return nil
}

// SetDeviceName changes the name shown to the other devices.
func SetDeviceName(home, name string) error {
	return edit(home, func(c *config.Config) error { c.DeviceName = config.Clean(name); return nil })
}

// SetQuota sets the maximum temporary storage of a relay folder, in MB.
func SetQuota(home, folderID string, quotaMB int64) error {
	return edit(home, func(c *config.Config) error {
		f := c.Folder(folderID)
		if f == nil {
			return fmt.Errorf("unknown folder %q", folderID)
		}
		f.QuotaMB = max(quotaMB, 16)
		return nil
	})
}

// SetRetentionHours sets after how long an undelivered change is dropped.
func SetRetentionHours(home, folderID string, hours int) error {
	return edit(home, func(c *config.Config) error {
		f := c.Folder(folderID)
		if f == nil {
			return fmt.Errorf("unknown folder %q", folderID)
		}
		f.RetentionHours = max(hours, 1)
		return nil
	})
}

// SetLANDiscovery turns local-network discovery on or off.
func SetLANDiscovery(home string, on bool) error {
	return edit(home, func(c *config.Config) error { c.LANDiscover = on; return nil })
}

// SetListenPort changes the port (UDP+TCP) the phone listens on.
func SetListenPort(home string, port int) error {
	if port < 1 || port > 65535 {
		return errors.New("invalid port")
	}
	return edit(home, func(c *config.Config) error { c.ListenPort = port; return nil })
}

// ---- running --------------------------------------------------------------------------

var (
	mu      sync.Mutex
	current *runner
	logs    = newRing(300)
)

// runner keeps the engine alive and restarts it when the configuration changes
// (pairing, edits) or the network changes.
type runner struct {
	home string
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	kick   chan struct{}

	rmu sync.Mutex // serialises restarts (supervisor tick, Restart, RemoveFolder)
	amu sync.Mutex
	a   *app.App
}

func (r *runner) app() *app.App {
	r.amu.Lock()
	defer r.amu.Unlock()
	return r.a
}

func (r *runner) set(a *app.App) {
	r.amu.Lock()
	r.a = a
	r.amu.Unlock()
}

func startApp(ctx context.Context, home string, log *slog.Logger) (*app.App, error) {
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return nil, err
	}
	return app.Start(ctx, home, cfg, id, log)
}

// Start runs the relay. It returns once the engine is listening; a supervisor
// then keeps it running until Stop.
func Start(home string) (err error) {
	defer guard(&err)
	mu.Lock()
	defer mu.Unlock()
	if current != nil {
		return errors.New("already running")
	}
	if _, err := Init(home, ""); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	// Take the reference version of the configuration *before* the engine loads it: a
	// change made right after Start returns must still be noticed by the supervisor.
	seen := cfgStamp(home)
	a, err := startApp(ctx, home, log)
	if err != nil {
		cancel()
		return err
	}
	r := &runner{home: home, log: log, ctx: ctx, cancel: cancel, done: make(chan struct{}), kick: make(chan struct{}, 1)}
	r.set(a)
	current = r
	go r.supervise(ctx, seen)
	return nil
}

// stamp identifies a version of config.json (modification time and size).
type stamp struct {
	mod  time.Time
	size int64
}

func cfgStamp(home string) stamp {
	fi, err := os.Stat(filepath.Join(home, app.ConfigFile))
	if err != nil {
		return stamp{}
	}
	return stamp{fi.ModTime(), fi.Size()}
}

// restart stops the engine and starts it again on the current configuration.
// It returns when the new engine is up (or has failed; the supervisor retries).
func (r *runner) restart() {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	r.log.Info("restarting engine")
	if a := r.app(); a != nil {
		a.Stop()
		r.set(nil)
	}
	a, err := startApp(r.ctx, r.home, r.log)
	if err != nil {
		r.log.Error("restart failed, will retry", "err", err)
		return
	}
	r.set(a)
}

func (r *runner) supervise(ctx context.Context, stamp stamp) {
	defer close(r.done)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		restart := false
		select {
		case <-ctx.Done():
			r.rmu.Lock()
			if a := r.app(); a != nil {
				a.Stop()
				r.set(nil)
			}
			r.rmu.Unlock()
			return
		case <-r.kick:
			restart = true
		case <-tick.C:
			restart = cfgStamp(r.home) != stamp || r.app() == nil // also retry a failed restart
		}
		if restart {
			stamp = cfgStamp(r.home)
			r.restart()
		}
	}
}

// Stop shuts the relay down and waits for it.
func Stop() {
	defer quiet()
	mu.Lock()
	r := current
	current = nil
	mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
	}
}

// Restart makes the engine rebind and redial; call it when the network changed
// (new Wi-Fi, mobile data, VPN).
func Restart() {
	defer quiet()
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// IsRunning reports whether the relay has been started (and not stopped). While
// the supervisor is retrying a failed restart it still counts as running: it is
// Stop, not Start, that ends it.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return current != nil
}

// StatusJSON returns what the UI shows: relay state, peers, addresses.
func StatusJSON() string {
	defer quiet()
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil || r.app() == nil {
		return `{"running":false}`
	}
	st := r.app().Status()
	b, err := json.Marshal(map[string]any{"running": true, "status": st})
	if err != nil {
		return `{"running":false}`
	}
	return string(b)
}

// SetLocalAddrs tells the engine which addresses this phone's network interfaces
// have (comma-separated IPs, from the system's LinkProperties). Android hides
// interface lists from apps on some versions; without this the phone could not
// announce an address a computer on the same Wi-Fi can reach. Call it before
// Start and whenever the network changes.
func SetLocalAddrs(csv string) {
	defer quiet()
	var ips []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" && len(ips) < 16 {
			ips = append(ips, s)
		}
	}
	localMu.Lock()
	localIPs = ips
	localMu.Unlock()
}

var (
	localMu  sync.Mutex
	localIPs []string
)

func init() {
	app.LocalIPs = func() []string {
		localMu.Lock()
		defer localMu.Unlock()
		return append([]string(nil), localIPs...)
	}
}

// Logs returns the most recent log lines, for the in-app diagnostics screen.
func Logs() string { defer quiet(); return logs.String() }
