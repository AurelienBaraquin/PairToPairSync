// Package config describes the on-disk configuration of a PairToPairSync device.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
)

const (
	RoleFull  = "full"  // keeps a complete copy of the folder in a normal directory
	RoleRelay = "relay" // temporary store-and-forward point (e.g. a phone)
)

// Device is a remote device we know about.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	Addresses []string `json:"addresses,omitempty"` // host:port; empty = rely on LAN discovery
}

// FolderDevice declares that a remote device takes part in a folder.
type FolderDevice struct {
	ID    string `json:"id"`
	Relay bool   `json:"relay,omitempty"` // that device is a temporary relay for this folder
}

// Folder is one synchronised folder.
type Folder struct {
	ID      string         `json:"id"`
	Label   string         `json:"label,omitempty"`
	Path    string         `json:"path"` // full: the synced directory; relay: the temporary storage directory
	Role    string         `json:"role,omitempty"`
	Devices []FolderDevice `json:"devices"`

	// Secret (base64, 32 bytes) is shared by the full devices of the folder only.
	// It encrypts everything that is handed to a relay, so a relay never sees file
	// names or content. It must never be configured on a relay.
	Secret string `json:"secret,omitempty"`

	// Full devices.
	ScanIntervalSec int `json:"scan_interval_sec,omitempty"` // safety full rescan, default 60

	// Relay devices.
	QuotaMB        int64 `json:"quota_mb,omitempty"`        // max temporary storage, default 2048
	RetentionHours int   `json:"retention_hours,omitempty"` // drop undelivered changes after this, default 720 (30 days)
}

type Config struct {
	DeviceName string `json:"device_name"`
	ListenPort int    `json:"listen_port"`        // UDP (QUIC) and TCP port, default 22000
	APIAddr    string `json:"api_addr,omitempty"` // local status page, default "127.0.0.1:8384", "-" disables
	// ListenAddresses overrides ListenPort with explicit multiaddrs (advanced).
	ListenAddresses []string `json:"listen_addresses,omitempty"`

	// Connectivity. Everything is peer to peer: no third-party server is ever
	// contacted. Only your own devices help each other to connect.
	LANDiscover  bool `json:"lan_discovery"` // find devices on the local network (mDNS)
	NATTraversal bool `json:"nat_traversal"` // UPnP/NAT-PMP port mapping, NAT detection and hole punching
	DHT          bool `json:"dht"`           // private address book shared by your own devices
	RelayService bool `json:"relay_service"` // let your other devices connect *through* this one when they cannot reach each other

	Devices []Device `json:"devices"`
	Folders []Folder `json:"folders"`
}

// Defaults are deliberately not 22000 and 8384: those are Syncthing's, a program
// many users of this kind of tool already run.
const (
	DefaultPort    = 22100
	DefaultAPIAddr = "127.0.0.1:8484"
)

func Default() *Config {
	host, _ := os.Hostname()
	return &Config{DeviceName: host, ListenPort: DefaultPort, APIAddr: DefaultAPIAddr,
		LANDiscover: true, NATTraversal: true, DHT: true, RelayService: true}
}

// Load reads the configuration; a missing file yields the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a mistyped key would otherwise be silently ignored
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}

// Save writes the configuration atomically and durably (see fsutil).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, b, 0o600)
}

// Validate normalises device IDs and checks references.
func (c *Config) Validate() error {
	if c.ListenPort < 0 || c.ListenPort > 65535 {
		return fmt.Errorf("listen_port %d is out of range", c.ListenPort)
	}
	known := map[string]bool{}
	for i := range c.Devices {
		id, err := identity.Normalize(c.Devices[i].ID)
		if err != nil {
			return err
		}
		c.Devices[i].ID = id
		c.Devices[i].Name = Clean(c.Devices[i].Name)
		known[id] = true
	}
	seen := map[string]bool{}
	for i := range c.Folders {
		f := &c.Folders[i]
		if f.Path == "" {
			return errors.New("folder needs a path")
		}
		// A relative path would be resolved against whatever directory the daemon
		// happens to start in (System32 for a scheduled task).
		if !filepath.IsAbs(f.Path) {
			return fmt.Errorf("folder %q: the path must be absolute, got %q", f.ID, f.Path)
		}
		if f.ScanIntervalSec < 0 || f.ScanIntervalSec > 86400 || f.RetentionHours < 0 || f.RetentionHours > 24*366 ||
			f.QuotaMB < 0 || f.QuotaMB > 1<<20 {
			return fmt.Errorf("folder %q: scan interval, retention or quota out of range", f.ID)
		}
		if !ValidFolderID(f.ID) {
			return fmt.Errorf("invalid folder id %q: use 1-64 letters, digits, '.', '_' or '-'", f.ID)
		}
		f.Label = Clean(f.Label)
		if seen[f.ID] {
			return fmt.Errorf("duplicate folder id %q", f.ID)
		}
		seen[f.ID] = true
		if f.Role == "" {
			f.Role = RoleFull
		}
		if f.Role != RoleFull && f.Role != RoleRelay {
			return fmt.Errorf("folder %q: unknown role %q", f.ID, f.Role)
		}
		if err := f.checkSecret(); err != nil {
			return err
		}
		for j := range f.Devices {
			id, err := identity.Normalize(f.Devices[j].ID)
			if err != nil {
				return err
			}
			f.Devices[j].ID = id
			if !known[id] {
				return fmt.Errorf("folder %q references unknown device %s", f.ID, id)
			}
		}
	}
	return nil
}

func (f *Folder) hasRelayPeer() bool {
	for _, d := range f.Devices {
		if d.Relay {
			return true
		}
	}
	return false
}

func (f *Folder) checkSecret() error {
	switch {
	case f.Role == RoleRelay && f.Secret != "":
		return fmt.Errorf("folder %q: a relay must not hold the folder secret (it would defeat end-to-end encryption)", f.ID)
	case f.Role == RoleFull && f.hasRelayPeer() && f.Secret == "":
		return fmt.Errorf("folder %q uses a relay but has no secret; run `p2psync folder add ... ` to generate one, "+
			"or copy the `secret` of the same folder from another full device", f.ID)
	}
	return nil
}

func (c *Config) Device(id string) *Device {
	for i := range c.Devices {
		if c.Devices[i].ID == id {
			return &c.Devices[i]
		}
	}
	return nil
}

func (c *Config) Folder(id string) *Folder {
	for i := range c.Folders {
		if c.Folders[i].ID == id {
			return &c.Folders[i]
		}
	}
	return nil
}

// SharesFolder reports whether device id takes part in folder f.
func (f *Folder) SharesWith(id string) bool {
	for _, d := range f.Devices {
		if d.ID == id {
			return true
		}
	}
	return false
}

// IsRelay reports whether device id is a relay for this folder.
func (f *Folder) IsRelay(id string) bool {
	for _, d := range f.Devices {
		if d.ID == id {
			return d.Relay
		}
	}
	return false
}

// FullDevices lists the non-relay devices of the folder (the ones that must
// eventually receive every change).
func (f *Folder) FullDevices() []string {
	var out []string
	for _, d := range f.Devices {
		if !d.Relay {
			out = append(out, d.ID)
		}
	}
	return out
}

func (f *Folder) ScanInterval() time.Duration {
	if f.ScanIntervalSec > 0 {
		return time.Duration(f.ScanIntervalSec) * time.Second
	}
	return time.Minute
}

func (f *Folder) Quota() int64 {
	if f.QuotaMB > 0 {
		return f.QuotaMB << 20
	}
	return 2048 << 20
}

func (f *Folder) Retention() time.Duration {
	if f.RetentionHours > 0 {
		return time.Duration(f.RetentionHours) * time.Hour
	}
	return 720 * time.Hour
}
