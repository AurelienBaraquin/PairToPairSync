package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// abs returns a path that is absolute on every platform.
func abs() string { return filepath.Join(os.TempDir(), "p2p-test-folder") }

const testID = "12D3KooWDAKocJYhkhxMyDZ4SL1SRTvkUagXCVpapAsz788yQNqX"

func TestRelayConfigMayNotContainSecret(t *testing.T) {
	c := &Config{Devices: []Device{{ID: testID}}, Folders: []Folder{{
		ID: "f", Path: abs(), Role: RoleRelay, Secret: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "relay must not hold") {
		t.Fatalf("expected rejection, got %v", err)
	}
}

func TestFullFolderWithRelayNeedsSecret(t *testing.T) {
	c := &Config{Devices: []Device{{ID: testID}}, Folders: []Folder{{
		ID: "f", Path: abs(), Role: RoleFull, Devices: []FolderDevice{{ID: testID, Relay: true}}}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "no secret") {
		t.Fatalf("a folder using a relay without a secret must be rejected, got %v", err)
	}
}

func TestFolderIDCannotEscapeADirectory(t *testing.T) {
	for _, bad := range []string{"", "x/../../../tmp/pwned", "../x", "a/b", `a\b`, "..", "a..b", ".hidden", "-x", "a b", "x\x00y", strings.Repeat("a", 65)} {
		if ValidFolderID(bad) {
			t.Errorf("%q accepted", bad)
		}
		c := &Config{Folders: []Folder{{ID: bad, Path: abs()}}}
		if c.Validate() == nil {
			t.Errorf("config with folder id %q validated", bad)
		}
	}
	for _, ok := range []string{"docs", "My-Files_2.0", "a"} {
		if !ValidFolderID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	if id := SlugID("../Mes Documents/été"); !ValidFolderID(id) {
		t.Errorf("slug %q invalid", id)
	}
}

func TestCleanStripsMarkup(t *testing.T) {
	got := Clean(`<img src=x onerror=alert(1)>"Bob's PC" & co`)
	if strings.ContainsAny(got, `<>"&`) || len(got) == 0 {
		t.Fatalf("markup survived: %q", got)
	}
	if len([]rune(Clean(strings.Repeat("é", 500)))) != 64 {
		t.Fatal("not truncated")
	}
}

func TestOverlappingFoldersAndPrivateDataAreRefused(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "p2psync")
	mk := func(paths ...string) *Config {
		c := &Config{}
		for i, p := range paths {
			c.Folders = append(c.Folders, Folder{ID: string(rune('a' + i)), Path: p})
		}
		return c
	}
	bad := map[string]*Config{
		"a folder around the private directory": mk(root),
		"the private directory itself":          mk(home),
		"a folder inside the private directory": mk(filepath.Join(home, "sync")),
		"identical folders":                     mk(filepath.Join(root, "docs"), filepath.Join(root, "docs")),
		"nested folders":                        mk(filepath.Join(root, "docs"), filepath.Join(root, "docs", "sub")),
		"nested the other way":                  mk(filepath.Join(root, "docs", "sub"), filepath.Join(root, "docs")),
	}
	for name, c := range bad {
		if err := c.CheckPaths(home); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := mk(filepath.Join(root, "docs"), filepath.Join(root, "docs2"), filepath.Join(root, "photos"))
	if err := ok.CheckPaths(home); err != nil {
		t.Errorf("sibling folders refused: %v", err)
	}
}

func TestUnknownConfigKeysAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"device_name": "x", "listen_prot": 22100}`), 0o600) // typo
	if _, err := Load(path); err == nil {
		t.Fatal("a mistyped key was silently ignored")
	}
}

func TestOutOfRangeValuesAreRejected(t *testing.T) {
	for name, c := range map[string]*Config{
		"port too large":   {ListenPort: 70000},
		"relative path":    {Folders: []Folder{{ID: "a", Path: "docs"}}},
		"huge interval":    {Folders: []Folder{{ID: "a", Path: abs(), ScanIntervalSec: 1 << 40}}},
		"negative quota":   {Folders: []Folder{{ID: "a", Path: abs(), QuotaMB: -1}}},
		"absurd retention": {Folders: []Folder{{ID: "a", Path: abs(), RetentionHours: 1 << 30}}},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
