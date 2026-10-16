package relay

import (
	"encoding/json"
	"fmt"
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

func sealedEntry(id string, version model.Vector) protocol.SealedEntry {
	return protocol.SealedEntry{ID: id, Version: version, Meta: []byte("sealed"), ModTime: 1} // a deletion-sized change: no blocks
}

// The race that used to leave data on the relay: B's acknowledgement by probe
// removes a change, then B's own announcement of the same version arrives. That
// must not look like a brand new change (the relay would fetch it again).
func TestLateAnnouncementOfADeliveredChangeIsIgnored(t *testing.T) {
	r := New(config.Folder{ID: "f", Path: t.TempDir(), Role: config.RoleRelay,
		Devices: []config.FolderDevice{{ID: "A"}, {ID: "B"}}}, "SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	id := "0123456789abcdef0123456789abcdef"
	v := model.Vector{"A": 5}

	r.ingest("A", []protocol.SealedEntry{sealedEntry(id, v)})
	r.mu.Lock()
	if r.pending[id] == nil {
		t.Fatal("change not recorded")
	}
	r.pending[id].Acked["B"] = true // acknowledged through a probe
	r.mu.Unlock()
	r.evaluate()
	r.mu.Lock()
	gone := r.pending[id] == nil
	r.mu.Unlock()
	if !gone {
		t.Fatal("delivered change was not removed")
	}

	r.ingest("B", []protocol.SealedEntry{sealedEntry(id, v)}) // B's announcement arrives late
	r.mu.Lock()
	_, resurrected := r.pending[id]
	r.mu.Unlock()
	if resurrected {
		t.Fatal("a late acknowledgement resurrected a delivered change")
	}

	// A genuinely newer version of the same file is a new change, of course.
	r.ingest("A", []protocol.SealedEntry{sealedEntry(id, model.Vector{"A": 6})})
	r.mu.Lock()
	_, isNew := r.pending[id]
	r.mu.Unlock()
	if !isNew {
		t.Fatal("a newer version was ignored")
	}
}

func TestDeliveredTableSurvivesRestartAndIsBounded(t *testing.T) {
	dir := t.TempDir()
	mk := func() *Relay {
		return New(config.Folder{ID: "f", Path: dir, Role: config.RoleRelay,
			Devices: []config.FolderDevice{{ID: "A"}, {ID: "B"}}}, "SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	id := "fedcba9876543210fedcba9876543210"
	r := mk()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.Start(t.Context()))
	r.ingest("A", []protocol.SealedEntry{sealedEntry(id, model.Vector{"A": 1})})
	r.mu.Lock()
	r.pending[id].Acked["B"] = true
	r.mu.Unlock()
	r.evaluate()
	r.Stop()

	r2 := mk()
	must(r2.Start(t.Context()))
	defer r2.Stop()
	r2.ingest("B", []protocol.SealedEntry{sealedEntry(id, model.Vector{"A": 1})})
	r2.mu.Lock()
	n := len(r2.pending)
	r2.mu.Unlock()
	if n != 0 {
		t.Fatal("delivery was forgotten across a restart")
	}

	old := deliveredMax
	deliveredMax = 3
	defer func() { deliveredMax = old }()
	r2.mu.Lock()
	for i := 0; i < 10; i++ {
		r2.delivered[fmt.Sprintf("%032d", i)] = delivery{Version: model.Vector{"A": 1}, At: time.Now().Add(time.Duration(i) * time.Second)}
	}
	r2.pruneDeliveredLocked(time.Now())
	size := len(r2.delivered)
	r2.mu.Unlock()
	if size != 3 {
		t.Fatalf("table not bounded: %d", size)
	}
}
