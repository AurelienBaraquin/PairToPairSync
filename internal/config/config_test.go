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
