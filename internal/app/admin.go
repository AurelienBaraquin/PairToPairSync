package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/seal"
)

// Init creates the identity and a default configuration if they do not exist,
// and returns the device ID.
func Init(home, deviceName string, port int, apiAddr string) (string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	// Keep the key and the folder secrets private to this user (a no-op chmod on
	// Unix; a restricted access list on Windows, whatever drive the user chose).
	_ = fsutil.Protect(home)
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ConfigFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := config.Default()
		if deviceName != "" {
			cfg.DeviceName = deviceName
		}
		if port != 0 {
			cfg.ListenPort = port
		}
		if apiAddr != "" {
			cfg.APIAddr = apiAddr
		}
		if err := cfg.Save(path); err != nil {
			return "", err
		}
	}
	return id.ID, nil
}

func LoadConfig(home string) (*config.Config, error) {
	cfg, err := config.Load(filepath.Join(home, ConfigFile))
	if err != nil {
		return nil, err
	}
	return cfg, cfg.CheckPaths(home)
}

func SaveConfig(home string, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.CheckPaths(home); err != nil {
		return err
	}
	return cfg.Save(filepath.Join(home, ConfigFile))
}

// NormalizePath turns what a user typed into an absolute path. Shells differ:
// PowerShell and cmd do not expand "~", and a quoted Windows path ending in a
// backslash ("C:\My Docs\") reaches the program as C:\My Docs" with a stray quote.
func NormalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(h, p[1:])
	}
	if p == "" {
		return "", errors.New("empty path")
	}
	return filepath.Abs(p)
}

// AddDevice registers (or updates) a remote device.
// ref is a bare device ID or a full address ending in /p2p/<ID>.
func AddDevice(cfg *config.Config, ref, name string, addrs []string) error {
	id, addr, err := identity.ParseRef(ref)
	if err != nil {
		return err
	}
	if addr != "" {
		addrs = append(addrs, addr)
	}
	name = config.Clean(name)
	if d := cfg.Device(id); d != nil {
		if name != "" {
			d.Name = name
		}
		for _, a := range addrs {
			if !slices.Contains(d.Addresses, a) {
				d.Addresses = append(d.Addresses, a)
			}
		}
		return nil
	}
	cfg.Devices = append(cfg.Devices, config.Device{ID: id, Name: name, Addresses: slices.Compact(addrs)})
	return nil
}

// ShareFolder lets a known device take part in a folder; relay marks it as a
// temporary relay rather than a full copy.
func ShareFolder(cfg *config.Config, folderID, rawDevID string, relay bool) error {
	f := cfg.Folder(folderID)
	if f == nil {
		return fmt.Errorf("unknown folder %q", folderID)
	}
	id, err := identity.Normalize(rawDevID)
	if err != nil {
		return err
	}
	if cfg.Device(id) == nil {
		return fmt.Errorf("device %s is not known; add it first", identity.Format(id))
	}
	for i := range f.Devices {
		if f.Devices[i].ID == id {
			f.Devices[i].Relay = relay
			return nil
		}
	}
	f.Devices = append(f.Devices, config.FolderDevice{ID: id, Relay: relay})
	return nil
}

// AddFolder declares a synchronised folder (role full) or a relay store (role relay).
//
// A full folder gets a secret (generated when none is supplied; pass the same
// one on every full device of the folder). It is returned so it can be shown to
// the user once. A relay never gets one.
func AddFolder(cfg *config.Config, id, path, role string, quotaMB int64, retentionHours int, secret string) (string, error) {
	abs, err := NormalizePath(path)
	if err != nil {
		return "", err
	}
	if id == "" {
		id = config.SlugID(filepath.Base(abs)) // from the absolute path: "." has no useful name
	}
	if !config.ValidFolderID(id) {
		return "", fmt.Errorf("invalid folder id %q: use 1-64 letters, digits, '.', '_' or '-'", id)
	}
	if cfg.Folder(id) != nil {
		return "", fmt.Errorf("folder %q already exists", id)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	if role == config.RoleRelay {
		secret = ""
	} else if secret == "" {
		if secret, err = seal.NewSecret(); err != nil {
			return "", err
		}
	} else if _, err := seal.ParseSecret(secret); err != nil {
		return "", err
	}
	cfg.Folders = append(cfg.Folders, config.Folder{
		ID: id, Label: config.Clean(filepath.Base(abs)), Path: abs, Role: role, QuotaMB: quotaMB, RetentionHours: retentionHours, Secret: secret,
	})
	return secret, nil
}
