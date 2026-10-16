// Package mobile is the API the Android application calls (through gomobile).
//
// It only uses types gomobile can bridge (string, int64, bool, error). The phone
// acts as a temporary relay: it stores undelivered changes in the app's private
// directory and forgets them once every full device has them. All state lives
// under the `home` directory the app passes in (its private files directory).
//
//	gomobile bind -target=android/arm64,android/amd64 -androidapi 26 \
//	    -ldflags="-checklinkname=0" -o p2psync.aar github.com/aurelienbaraquin/pairtopairsync/mobile
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

// Version of the embedded engine.
func Version() string { return "0.3.0" }

// ---- identity ------------------------------------------------------------------------

// Init creates the identity and a default configuration on first use and returns
// the device ID. It is idempotent.
func Init(home, deviceName string) (string, error) {
	id, err := app.Init(home, config.Clean(deviceName), 0, "-") // "-": no local web page on a phone
	if err != nil {
		return "", err
	}
	return identity.Format(id), nil
}

// DeviceID returns this device's ID ("" if not initialised).
func DeviceID(home string) string {
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
func Pair(home, code, storageDir string, quotaMB int64) (string, error) {
	inv, err := pairing.Decode(code)
	if err != nil {
		return "", err
	}
	if inv.Expired(time.Now()) {
		return "", errors.New("this invitation has expired; ask for a new one")
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
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	resp, err := pairing.Dial(ctx, id.Key, inv, app.GuestRequest(cfg))
	if err != nil {
		return "", err
	}
	// Nothing was changed on disk until the other device accepted us.
	if err := app.Join(cfg, inv, resp, opts); err != nil {
		return "", err
	}
	if err := app.SaveConfig(home, cfg); err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]string{"folder": inv.Folder, "label": inv.Label, "peer": resp.Name, "peer_id": inv.ID})
	return string(b), nil
}

// NewInvite creates a single-use invitation for a full device that should use
// this phone as its relay (the phone shows the QR, the computer scans it with
// `p2psync pair`). The engine must be running. It returns the invitation text.
func NewInvite(home, folderID string) (string, error) {
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
	ID   string `json:"id"`
	Name string `json:"name"`
}

type folderView struct {
	ID             string       `json:"id"`
	Label          string       `json:"label"`
	Role           string       `json:"role"`
	QuotaMB        int64        `json:"quota_mb"`
	RetentionHours int          `json:"retention_hours"`
	Devices        []deviceView `json:"devices"`
}

// ConfigJSON describes devices and folders for the UI. It never contains secrets.
func ConfigJSON(home string) (string, error) {
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
		out.Devices = append(out.Devices, deviceView{d.ID, d.Name})
	}
	for _, f := range cfg.Folders {
		fv := folderView{ID: f.ID, Label: f.Label, Role: f.Role, QuotaMB: f.Quota() >> 20,
			RetentionHours: int(f.Retention().Hours()), Devices: []deviceView{}}
		for _, fd := range f.Devices {
			name := fd.ID
			if d := cfg.Device(fd.ID); d != nil && d.Name != "" {
				name = d.Name
			}
			fv.Devices = append(fv.Devices, deviceView{fd.ID, name})
		}
		out.Folders = append(out.Folders, fv)
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func edit(home string, fn func(*config.Config) error) error {
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return app.SaveConfig(home, cfg)
}

// RemoveDevice forgets a device everywhere. Pending changes it had not yet
// received no longer hold anything back.
func RemoveDevice(home, deviceID string) error {
	id, err := identity.Normalize(deviceID)
	if err != nil {
		return err
	}
	return edit(home, func(c *config.Config) error {
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
func RemoveFolder(home, folderID string) error {
	var path string
	err := edit(home, func(c *config.Config) error {
		var keep []config.Folder
		for _, f := range c.Folders {
			if f.ID == folderID {
				path = f.Path
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

	cancel context.CancelFunc
	done   chan struct{}
	kick   chan struct{}

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
func Start(home string) error {
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
	a, err := startApp(ctx, home, log)
	if err != nil {
		cancel()
		return err
	}
	r := &runner{home: home, log: log, cancel: cancel, done: make(chan struct{}), kick: make(chan struct{}, 1)}
	r.set(a)
	current = r
	go r.supervise(ctx)
	return nil
}

func cfgStamp(home string) time.Time {
	fi, err := os.Stat(filepath.Join(home, app.ConfigFile))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (r *runner) supervise(ctx context.Context) {
	defer close(r.done)
	stamp := cfgStamp(r.home)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		restart := false
		select {
		case <-ctx.Done():
			if a := r.app(); a != nil {
				a.Stop()
			}
			return
		case <-r.kick:
			restart = true
		case <-tick.C:
			restart = cfgStamp(r.home) != stamp || r.app() == nil // also retry a failed restart
		}
		if !restart {
			continue
		}
		r.log.Info("restarting engine")
		if a := r.app(); a != nil {
			a.Stop()
			r.set(nil)
		}
		stamp = cfgStamp(r.home)
		a, err := startApp(ctx, r.home, r.log)
		if err != nil {
			r.log.Error("restart failed, will retry", "err", err)
			continue
		}
		r.set(a)
	}
}

// Stop shuts the relay down and waits for it.
func Stop() {
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

// IsRunning reports whether the engine is started.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return current != nil && current.app() != nil
}

// StatusJSON returns what the UI shows: relay state, peers, addresses.
func StatusJSON() string {
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

// Logs returns the most recent log lines, for the in-app diagnostics screen.
func Logs() string { return logs.String() }
