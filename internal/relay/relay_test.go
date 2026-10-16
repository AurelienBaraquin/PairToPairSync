package relay

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// A change nobody collected within the retention period must not stay on the
// phone forever: the relay is not meant to be permanent storage.
func TestExpiredChangesAreDroppedWithTheirData(t *testing.T) {
	dir := t.TempDir()
	data := []byte("some data")
	hash := model.HashBytes(data)
	r := New(config.Folder{ID: "f", Path: dir, Role: config.RoleRelay, RetentionHours: 1,
		Devices: []config.FolderDevice{{ID: "DEV1"}}}, "SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.dir = dir

	id := "0123456789abcdef0123456789abcdef"
	pending := map[string]*Pending{
		id: {Entry: protocol.SealedEntry{ID: id, Size: int64(len(data)), BlockSize: 128 << 10,
			CBlocks: [][]byte{hash}, Version: model.Vector{"X": 1}, Meta: []byte("sealed")},
			Acked: map[string]bool{"X": true}, Have: true, Since: time.Now().Add(-2 * time.Hour)},
	}
	b, _ := json.Marshal(pending)
	os.WriteFile(filepath.Join(dir, "pending.json"), b, 0o600)
	blk := r.blockPath(hash)
	os.MkdirAll(filepath.Dir(blk), 0o700)
	os.WriteFile(blk, data, 0o600)

	TickInterval = 20 * time.Millisecond
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.pending)
		r.mu.Unlock()
		if _, err := os.Stat(blk); n == 0 && os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expired change and its data block were not removed")
}

func TestStartIgnoresNothingOnEmptyStore(t *testing.T) {
	r := New(config.Folder{ID: "f", Path: t.TempDir(), Role: config.RoleRelay}, "SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.Stop()
}

// The relay carries ciphertext it has no way to read. This pins that down at
// the build level: the package must never depend on the encryption package, so
// adding a decryption path to the relay requires changing this test on purpose.
func TestRelayCannotDecrypt(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Skip("go tool unavailable:", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasSuffix(dep, "/internal/seal") || strings.HasSuffix(dep, "/internal/folder") {
			t.Fatalf("relay depends on %s: it must not be able to open sealed data", dep)
		}
	}
}

func TestRelayRefusesToHoldTheSecret(t *testing.T) {
	r := New(config.Folder{ID: "f", Path: t.TempDir(), Role: config.RoleRelay, Secret: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"},
		"SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Start(t.Context()); err == nil {
		r.Stop()
		t.Fatal("relay started with the folder secret")
	}
}
