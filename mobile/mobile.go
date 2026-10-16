// Package mobile exposes a minimal, gomobile-friendly API (only strings, ints
// and errors) so that an Android/iOS application can embed PairToPairSync and
// run the phone as a temporary relay:
//
//	gomobile bind -target=android -o p2psync.aar github.com/aurelienbaraquin/pairtopairsync/mobile
//
// The host app is expected to run Start from a foreground service.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
)

var (
	mu      sync.Mutex
	running *app.App
)

// Init creates the identity if needed and returns the (formatted) device ID.
func Init(home, deviceName string) (string, error) {
	id, err := app.Init(home, deviceName, "", "-")
	if err != nil {
		return "", err
	}
	return identity.Format(id), nil
}

// AddDevice registers a remote device (addr may be empty to rely on LAN discovery).
func AddDevice(home, deviceID, name, addr string) error {
	return edit(home, func(c *config.Config) error {
		var addrs []string
		if addr != "" {
			addrs = []string{addr}
		}
		return app.AddDevice(c, deviceID, name, addrs)
	})
}

// AddRelayFolder makes this phone a temporary relay for folderID, storing
// undelivered changes in storagePath (use the app's private directory).
func AddRelayFolder(home, folderID, storagePath string, quotaMB int64) error {
	return edit(home, func(c *config.Config) error {
		return app.AddFolder(c, folderID, storagePath, config.RoleRelay, quotaMB, 0)
	})
}

// ShareFolder lets a known device take part in folderID.
func ShareFolder(home, folderID, deviceID string, deviceIsRelay bool) error {
	return edit(home, func(c *config.Config) error {
		return app.ShareFolder(c, folderID, deviceID, deviceIsRelay)
	})
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

// Start runs the device in the background.
func Start(home string) error {
	mu.Lock()
	defer mu.Unlock()
	if running != nil {
		return errors.New("already running")
	}
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return err
	}
	a, err := app.Start(context.Background(), home, cfg, id, nil)
	if err != nil {
		return err
	}
	running = a
	return nil
}

// Stop shuts the device down.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if running != nil {
		running.Stop()
		running = nil
	}
}

// StatusJSON returns the same document as the local status API.
func StatusJSON() string {
	mu.Lock()
	defer mu.Unlock()
	if running == nil {
		return "{}"
	}
	b, _ := json.Marshal(running.Status())
	return string(b)
}
