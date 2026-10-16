// Package app wires configuration, identity, folder engines, networking and
// the local status API into one running device.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/api"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/folder"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/relay"
)

const ConfigFile = "config.json"

// RedialMax caps the reconnection back-off (tests shorten it).
var RedialMax time.Duration

// NodeHook lets tests adjust the network options (e.g. force reachability).
var NodeHook func(*node.Options)

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

	opts := node.Options{Config: cfg, Identity: id, Handlers: a.handlers, Logger: log, RedialMax: RedialMax,
		Pairing: newPairer(home, log)}
	if NodeHook != nil {
		NodeHook(&opts)
	}
	a.Node = node.New(opts)
	if err := a.Node.Start(ctx); err != nil {
		a.Stop()
		return nil, err
	}
	if cfg.APIAddr != "" && cfg.APIAddr != "-" {
		tok, terr := APIToken(home)
		if terr != nil {
			log.Warn("status API unavailable", "err", terr)
			return a, nil
		}
		srv, err := api.Start(cfg.APIAddr, tok, a.Status, a.inviteForAPI, log)
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
}

// Status is the document served by the local API.
type Status struct {
	DeviceID   string          `json:"device_id"`
	DeviceName string          `json:"device_name"`
	Addresses  []string        `json:"addresses"`
	Folders    []any           `json:"folders"`
	Peers      []node.PeerInfo `json:"peers"`
}

func (a *App) Status() any {
	st := Status{DeviceID: identity.Format(a.Identity.ID), DeviceName: a.Cfg.DeviceName, Addresses: a.Node.Addrs(), Peers: a.Node.Peers()}
	for _, h := range a.handlers {
		st.Folders = append(st.Folders, h.Status())
	}
	return st
}

// inviteForAPI creates an invitation on behalf of the local web page.
func (a *App) inviteForAPI(folder string, asRelay bool) (api.InviteResult, error) {
	cfg, err := LoadConfig(a.Home)
	if err != nil {
		return api.InviteResult{}, err
	}
	inv, err := CreateInvite(a.Home, cfg, a.Identity.ID, a.Node.Addrs(), folder, asRelay, 0)
	if err != nil {
		return api.InviteResult{}, err
	}
	code := inv.Encode()
	png, err := pairing.QRPNG(code, 320)
	if err != nil {
		return api.InviteResult{}, err
	}
	return api.InviteResult{Code: code, PNG: png, Expires: time.Unix(inv.Exp, 0)}, nil
}

// APIToken returns the secret that protects the local status API, creating it
// (0600) on first use. Only someone who can read the user's config directory can
// talk to the API.
func APIToken(home string) (string, error) {
	path := filepath.Join(home, "api-token")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok), 0o600)
}
