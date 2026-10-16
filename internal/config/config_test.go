package config

import (
	"strings"
	"testing"
)

const testID = "12D3KooWDAKocJYhkhxMyDZ4SL1SRTvkUagXCVpapAsz788yQNqX"

func TestRelayConfigMayNotContainSecret(t *testing.T) {
	c := &Config{Devices: []Device{{ID: testID}}, Folders: []Folder{{
		ID: "f", Path: "/x", Role: RoleRelay, Secret: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "relay must not hold") {
		t.Fatalf("expected rejection, got %v", err)
	}
}

func TestFullFolderWithRelayNeedsSecret(t *testing.T) {
	c := &Config{Devices: []Device{{ID: testID}}, Folders: []Folder{{
		ID: "f", Path: "/x", Role: RoleFull, Devices: []FolderDevice{{ID: testID, Relay: true}}}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "no secret") {
		t.Fatalf("a folder using a relay without a secret must be rejected, got %v", err)
	}
}

func TestFolderIDCannotEscapeADirectory(t *testing.T) {
	for _, bad := range []string{"", "x/../../../tmp/pwned", "../x", "a/b", `a\b`, "..", "a..b", ".hidden", "-x", "a b", "x\x00y", strings.Repeat("a", 65)} {
		if ValidFolderID(bad) {
			t.Errorf("%q accepted", bad)
		}
		c := &Config{Folders: []Folder{{ID: bad, Path: "/x"}}}
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
