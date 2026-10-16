// Package app wires configuration, identity, folder engines, networking and
// the local status API into one running device.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/api"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/discovery"
	"github.com/aurelienbaraquin/pairtopairsync/internal/folder"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/relay"
)

const ConfigFile = "config.json"

// RedialMax caps the reconnection back-off (tests shorten it).
var RedialMax time.Duration

// DefaultHome returns the per-user directory holding config, identity and indexes.
func DefaultHome() (string, error) {
	if h := os.Getenv("P2PSYNC_HOME"); h != "" {
		return h, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "p2psync"), nil
}

type App struct {
	Home     string
	Cfg      *config.Config
	Identity *identity.Identity
	Node     *node.Node

	handlers []node.FolderHandler
	disc     *discovery.Discovery
	api      *api.Server
	log      *slog.Logger
}

// Start launches a device from its configuration.
func Start(ctx context.Context, home string, cfg *config.Config, id *identity.Identity, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	a := &App{Home: home, Cfg: cfg, Identity: id, log: log}
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	for _, fc := range cfg.Folders {
		switch fc.Role {
		case config.RoleRelay:
			a.handlers = append(a.handlers, relay.New(fc, id.ID, log))
		default:
			f, err := folder.New(fc, id.ID, stateDir, log)
			if err != nil {
				return nil, fmt.Errorf("folder %s: %w", fc.ID, err)
			}
			a.handlers = append(a.handlers, f)
		}
	}

	opts := node.Options{Config: cfg, Identity: id, Handlers: a.handlers, Logger: log, RedialMax: RedialMax}
	if cfg.LANDiscover {
		if _, port, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
			var p int
			fmt.Sscanf(port, "%d", &p)
			if d, err := discovery.Start(id.ID, p, log); err == nil {
				a.disc = d
				opts.Addresses = d
			} else {
				log.Warn("LAN discovery unavailable", "err", err)
			}
		}
	}
	a.Node = node.New(opts)
	if err := a.Node.Start(ctx); err != nil {
		a.Stop()
		return nil, err
	}
	if cfg.APIAddr != "" && cfg.APIAddr != "-" {
		srv, err := api.Start(cfg.APIAddr, a.Status, log)
		if err != nil {
			log.Warn("status API unavailable", "err", err)
		} else {
			a.api = srv
		}
	}
	return a, nil
}

func (a *App) Stop() {
	if a.api != nil {
		a.api.Stop()
	}
	if a.Node != nil {
		a.Node.Stop()
	}
	if a.disc != nil {
		a.disc.Stop()
	}
}

// Status is the document served by the local API.
type Status struct {
	DeviceID   string          `json:"device_id"`
	DeviceName string          `json:"device_name"`
	Folders    []any           `json:"folders"`
	Peers      []node.PeerInfo `json:"peers"`
}

func (a *App) Status() any {
	st := Status{DeviceID: identity.Format(a.Identity.ID), DeviceName: a.Cfg.DeviceName, Peers: a.Node.Peers()}
	for _, h := range a.handlers {
		st.Folders = append(st.Folders, h.Status())
	}
	return st
}
