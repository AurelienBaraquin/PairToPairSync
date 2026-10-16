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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/api"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/folder"
	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
	"github.com/aurelienbaraquin/pairtopairsync/internal/relay"
	"github.com/aurelienbaraquin/pairtopairsync/internal/seal"
)

const ConfigFile = "config.json"

// RedialMax caps the reconnection back-off (tests shorten it).
var RedialMax time.Duration

// NodeHook lets tests adjust the network options (e.g. force reachability).
var NodeHook func(*node.Options)

// LocalIPs, if set, supplies the device's interface addresses (see node.Options.LocalIPs).
var LocalIPs func() []string

// DefaultHome returns the per-user directory holding config, identity and indexes.
func DefaultHome() (string, error) {
	if h := os.Getenv("P2PSYNC_HOME"); h != "" {
		return h, nil
	}
	// On Windows use the machine-local profile (%LOCALAPPDATA%), not the roaming one
	// (%APPDATA%): a roaming profile would copy the device key and the folder secrets
	// to a server, and two PCs sharing a profile would end up with the same identity.
	dir, err := os.UserConfigDir()
	if runtime.GOOS == "windows" {
		dir, err = os.UserCacheDir()
	}
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

	Net *Membership

	handlers []node.FolderHandler
	pairer   *pairer
	api      *api.Server
	log      *slog.Logger
	stop     context.CancelFunc
	wg       sync.WaitGroup
}

// Start launches a device from its configuration.
func Start(ctx context.Context, home string, cfg *config.Config, id *identity.Identity, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	a := &App{Home: home, Cfg: cfg, Identity: id, log: log, Net: LoadMembership(home, id, log)}
	// Describe the devices this one was paired with in signed introductions before
	// anyone connects (a relay holds no full copy and introduces nobody).
	if _, err := a.Net.IssueFromConfig(cfg); err != nil {
		log.Warn("could not issue the introductions", "err", err)
	}
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	for _, fc := range cfg.Folders {
		if fc.AwaitingSecret {
			continue // started once a full device of the folder has handed the secret over
		}
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
		LocalIPs: LocalIPs}
	a.pairer = newPairer(home, log, a.Net)
	opts.Pairing = a.pairer
	opts.Network = &netHandler{a: a, times: map[string][]time.Time{}}
	opts.Secrets = a.secretFor
	if NodeHook != nil {
		NodeHook(&opts)
	}
	a.Node = node.New(opts)
	a.pairer.addrsOf = a.Node.AddrsOf
	if err := a.Node.Start(ctx); err != nil {
		a.Stop()
		return nil, err
	}
	var runCtx context.Context
	runCtx, a.stop = context.WithCancel(ctx)
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.fetchSecrets(runCtx) }()
	if cfg.APIAddr != "" && cfg.APIAddr != "-" {
		tok, terr := APIToken(home)
		if terr != nil {
			log.Warn("status API unavailable", "err", terr)
			return a, nil
		}
		srv, err := api.Start(cfg.APIAddr, tok, api.Hooks{Status: a.Status, Invite: a.inviteForAPI, NetInvite: a.netInviteForAPI,
			Requests: a.requestsForAPI, Decide: a.Decide}, log)
		if err != nil {
			log.Warn("status API unavailable", "err", err)
		} else {
			a.api = srv
		}
	}
	return a, nil
}

func (a *App) Stop() {
	if a.stop != nil {
		a.stop()
		a.wg.Wait()
	}
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
	Requests   []Request       `json:"requests"`
}

func (a *App) Status() any {
	st := Status{DeviceID: identity.Format(a.Identity.ID), DeviceName: a.Cfg.DeviceName, Addresses: a.Node.Addrs(), Peers: a.Node.Peers(),
		Requests: a.pairer.Requests()}
	for _, h := range a.handlers {
		st.Folders = append(st.Folders, h.Status())
	}
	for _, f := range a.Cfg.Folders {
		if f.AwaitingSecret {
			st.Folders = append(st.Folders, map[string]any{"id": f.ID, "label": f.Label, "role": f.Role, "path": f.Path,
				"state": "waiting for the folder secret", "managed": true})
		}
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
	return tok, fsutil.WriteFileAtomic(path, []byte(tok), 0o600)
}

func (a *App) requestsForAPI() any { return a.pairer.Requests() }

// netInviteForAPI creates a network invitation for the local web page.
func (a *App) netInviteForAPI(folders []string, asRelay, approve bool) (api.InviteResult, error) {
	cfg, err := LoadConfig(a.Home)
	if err != nil {
		return api.InviteResult{}, err
	}
	inv, err := CreateNetworkInvite(a.Home, cfg, a.Identity.ID, a.Node.Addrs(),
		NetworkInviteOptions{Folders: folders, AsRelay: asRelay, Approve: approve})
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

// secretFor answers a device asking for a folder's secret: only a device that
// this one lists as a full member of the folder gets it. (A relay never does.)
func (a *App) secretFor(requester, folderID string) (string, bool) {
	f := a.Cfg.Folder(folderID)
	if f == nil || f.Role != config.RoleFull || f.Secret == "" || f.AwaitingSecret {
		return "", false
	}
	if !f.SharesWith(requester) || f.IsRelay(requester) {
		return "", false
	}
	return f.Secret, true
}

// fetchSecrets asks the full devices of a folder for its secret while it is missing.
func (a *App) fetchSecrets(ctx context.Context) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	for {
		for _, f := range a.Cfg.Folders {
			if !f.AwaitingSecret {
				continue
			}
			for _, dev := range f.FullDevices() {
				if ctx.Err() != nil {
					return
				}
				sec, err := a.Node.FetchSecret(ctx, dev, f.ID)
				if err != nil {
					continue
				}
				if a.storeSecret(f.ID, sec) {
					return // the configuration changed: the daemon restarts on its own
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) storeSecret(folderID, secret string) (stored bool) {
	if _, err := seal.ParseSecret(secret); err != nil {
		return false
	}
	WithConfigLock(func() {
		cfg, err := LoadConfig(a.Home)
		if err != nil {
			return
		}
		f := cfg.Folder(folderID)
		if f == nil || !f.AwaitingSecret || f.Role != config.RoleFull {
			return
		}
		f.Secret, f.AwaitingSecret = secret, false
		if err := SaveConfig(a.Home, cfg); err != nil {
			a.log.Error("could not store the folder secret", "folder", folderID, "err", err)
			return
		}
		a.log.Info("folder secret received; starting the folder", "folder", folderID)
		stored = true
	})
	return stored
}

// netHandler gossips signed grants over the sessions.
type netHandler struct {
	a     *App
	mu    sync.Mutex
	times map[string][]time.Time
}

func (h *netHandler) Outgoing(peerID string) protocol.Grants {
	a := h.a
	return protocol.Grants{List: a.Net.ForPeer(a.Cfg, peerID), Hints: a.pairer.hintsFor(a.Cfg, peerID)}
}

func (h *netHandler) Incoming(peerID string, g protocol.Grants) {
	// A connected device cannot make this one re-read its configuration in a loop: a
	// handful of updates per minute is plenty (one per session, and sessions are rare).
	h.mu.Lock()
	recent := h.times[peerID][:0]
	for _, t := range h.times[peerID] {
		if time.Since(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 8 {
		h.times[peerID] = recent
		h.mu.Unlock()
		return
	}
	h.times[peerID] = append(recent, time.Now())
	h.mu.Unlock()
	for id, addrs := range g.Hints {
		h.a.Node.AddHints(id, addrs)
	}
	h.a.Net.Receive(peerID, g.List, g.Hints)
}

// Requests lists the devices waiting for this device's owner to let them join.
func (a *App) Requests() []Request { return a.pairer.Requests() }

// Decide accepts or declines one of them; it reports whether the request still existed.
func (a *App) Decide(id string, accept bool) bool { return a.pairer.Decide(id, accept) }
