package relay

import (
	"bytes"
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

func newTestRelay(t *testing.T, dir string) *Relay {
	t.Helper()
	return New(config.Folder{ID: "f", Path: dir, Role: config.RoleRelay,
		Devices: []config.FolderDevice{{ID: "A"}, {ID: "B"}}}, "SELF", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A power cut can truncate pending.json. That must not stop the device from
// starting (it would take every folder down with it).
func TestCorruptPendingFileIsSetAsideNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pending.json"), []byte(`{"abc": {"entry": {"id": "tru`), 0o600)
	r := newTestRelay(t, dir)
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("a corrupt pending.json prevented startup: %v", err)
	}
	defer r.Stop()
	if len(r.pending) != 0 {
		t.Fatal("expected an empty table")
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "pending.json.corrupt-*"))
	if len(kept) != 1 {
		t.Fatalf("the damaged file should be kept for inspection, found %v", kept)
	}
}

// After a crash, metadata may claim data that is not on disk. It must be re-checked.
func TestMissingBlocksAreDetectedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	r := newTestRelay(t, dir)
	id := "0123456789abcdef0123456789abcdef"
	h := bytes.Repeat([]byte{7}, 32)
	pending := map[string]*Pending{id: {Entry: protocol.SealedEntry{ID: id, Size: 10, BlockSize: 128 << 10,
		CBlocks: [][]byte{h}, Version: model.Vector{"A": 1}, Meta: []byte("m")}, Acked: map[string]bool{"A": true}, Have: true, Since: time.Now()}}
	b, _ := json.Marshal(pending)
	os.WriteFile(filepath.Join(dir, "pending.json"), b, 0o600)
	// A short block: the file exists but is truncated.
	os.MkdirAll(filepath.Dir(r.blockPath(h)), 0o700)
	os.WriteFile(r.blockPath(h), []byte("short"), 0o600)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if r.pending[id].Have {
		t.Fatal("a truncated block was trusted as complete")
	}
	// A complete one is accepted.
	full := make([]byte, 10+protocol.SealOverhead)
	os.WriteFile(r.blockPath(h), full, 0o600)
	r.mu.Lock()
	r.verifyBlocksLocked()
	r.mu.Unlock()
	if !r.pending[id].Have {
		t.Fatal("a complete block was not recognised")
	}
}

// The sender may only forget a change once the relay has really stored it.
func TestNoAcknowledgementWhenTheChangeCannotBeStored(t *testing.T) {
	dir := t.TempDir()
	r := newTestRelay(t, dir)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if _, ok := r.ingest("A", []protocol.SealedEntry{sealedEntry("0123456789abcdef0123456789abcdef", model.Vector{"A": 1})}); !ok {
		t.Fatal("a normal change must be acknowledged")
	}
	// Make storing impossible: a directory where the file should go.
	os.Remove(filepath.Join(dir, "pending.json"))
	os.Mkdir(filepath.Join(dir, "pending.json"), 0o700)
	if _, ok := r.ingest("A", []protocol.SealedEntry{sealedEntry("fedcba9876543210fedcba9876543210", model.Vector{"A": 1})}); ok {
		t.Fatal("a change that could not be stored was acknowledged: the sender would forget it")
	}
}

func TestPendingTableIsBounded(t *testing.T) {
	old := maxPending
	maxPending = 3
	defer func() { maxPending = old }()
	r := newTestRelay(t, t.TempDir())
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	var es []protocol.SealedEntry
	for i := 0; i < 6; i++ {
		es = append(es, sealedEntry(fmt.Sprintf("%032d", i), model.Vector{"A": 1}))
	}
	if _, ok := r.ingest("A", es); ok {
		t.Fatal("entries beyond the cap must not be acknowledged")
	}
	if len(r.pending) != 3 {
		t.Fatalf("table holds %d entries, cap is 3", len(r.pending))
	}
}

// The garbage collector must not delete a block that is still being downloaded.
func TestGCSparesDownloadsInProgress(t *testing.T) {
	r := newTestRelay(t, t.TempDir())
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	dir := filepath.Join(r.dir, "blocks", "ab")
	os.MkdirAll(dir, 0o700)
	young, old := filepath.Join(dir, "abcd.1.tmp"), filepath.Join(dir, "abcd.2.tmp")
	os.WriteFile(young, []byte("x"), 0o600)
	os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-2 * partGrace)
	os.Chtimes(old, past, past)
	r.mu.Lock()
	r.gcBlocksLocked()
	r.mu.Unlock()
	if _, err := os.Stat(young); err != nil {
		t.Fatal("a download in progress was deleted")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an abandoned temporary file was not cleaned up")
	}
}
