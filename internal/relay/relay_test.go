package relay

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
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

	pending := map[string]*Pending{
		"old.txt": {Entry: model.FileInfo{Name: "old.txt", Size: int64(len(data)), BlockSize: 128 << 10,
			Blocks: [][]byte{hash}, Version: model.Vector{"X": 1}}, Acked: map[string]bool{"X": true}, Have: true,
			Since: time.Now().Add(-2 * time.Hour)},
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
