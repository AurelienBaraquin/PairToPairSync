package folder

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

func newTestFolder(t *testing.T) *Folder {
	t.Helper()
	root := t.TempDir()
	f, err := New(config.Folder{ID: "t", Path: root, Role: config.RoleFull}, "SELFSELFSELF", t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	t.Cleanup(f.cancel)
	if err := os.MkdirAll(filepath.Join(root, MetaDir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// fileEntry describes content as the engine would index it.
func fileEntry(t *testing.T, name, content string, v model.Vector) model.FileInfo {
	t.Helper()
	bs, blocks, n, err := model.HashBlocks(strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return model.FileInfo{Name: name, Type: model.TypeFile, Size: n, ModTime: time.Now().UnixNano(), Mode: 0o644,
		BlockSize: bs, Blocks: blocks, Version: v}
}

// writeIndexed writes the file and records it as the local version, with a
// matching timestamp, as a completed scan would.
func writeIndexed(t *testing.T, f *Folder, name, content string, v model.Vector) model.FileInfo {
	t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(name))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	e := fileEntry(t, name, content, v)
	e.ModTime = fi.ModTime().UnixNano()
	return f.store.Put(e)
}

func read(t *testing.T, f *Folder, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(name)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// The bug: a change queued earlier was applied without looking at what the
// scanner recorded since, so a newer local edit could be overwritten or deleted.
func TestQueuedChangeIsDecidedAgainstTheCurrentLocalVersion(t *testing.T) {
	f := newTestFolder(t)

	// Local file edited to v3 = {A:1, SELF:1}; the queued remote change is {A:2}, concurrent with it.
	writeIndexed(t, f, "doc.txt", "my newer edit", model.Vector{"A": 1, "SELF": 1})

	// 1. a queued remote *deletion* must not delete the edited file (a modification beats a deletion)
	del := model.FileInfo{Name: "doc.txt", Type: model.TypeFile, Deleted: true, ModTime: time.Now().UnixNano() + 1e9, Version: model.Vector{"A": 2}}
	if err := f.apply(&want{entry: del, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, f, "doc.txt") != "my newer edit" {
		t.Fatal("a queued deletion destroyed a newer local edit")
	}

	// 2. an older queued version must be dropped, not applied over the newer local one
	old := fileEntry(t, "doc.txt", "old remote", model.Vector{"A": 1})
	if err := f.apply(&want{entry: old, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, f, "doc.txt") != "my newer edit" {
		t.Fatal("an outdated download overwrote a newer local version")
	}

	// 3. the same version we already hold is a no-op
	same := fileEntry(t, "doc.txt", "my newer edit", model.Vector{"A": 1, "SELF": 1})
	if err := f.apply(&want{entry: same, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}

	// 4. a concurrent change that loses the tie-break is dropped (the other device will come to us)
	loser := fileEntry(t, "doc.txt", "remote that loses", model.Vector{"A": 2})
	loser.ModTime = 1 // far older than ours: it loses
	if err := f.apply(&want{entry: loser, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, f, "doc.txt") != "my newer edit" {
		t.Fatal("a losing concurrent version overwrote the local file")
	}
}

func TestConcurrentIdenticalContentMergesWithoutTransfer(t *testing.T) {
	f := newTestFolder(t)
	local := writeIndexed(t, f, "same.txt", "identical", model.Vector{"SELF": 1})
	remote := fileEntry(t, "same.txt", "identical", model.Vector{"A": 1})
	if err := f.apply(&want{entry: remote, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.Get("same.txt")
	if !got.Version.Dominates(local.Version) || !got.Version.Dominates(remote.Version) {
		t.Fatalf("versions not merged: %v", got.Version)
	}
	if m, _ := filepath.Glob(filepath.Join(f.root, "*conflict*")); len(m) != 0 {
		t.Fatalf("spurious conflict copies: %v", m)
	}
}

// An unmodified file turning into a directory is a replacement, not a conflict.
func TestFileReplacedByDirectoryLeavesNoConflictCopy(t *testing.T) {
	f := newTestFolder(t)
	writeIndexed(t, f, "thing", "plain file", model.Vector{"A": 1})
	dir := model.FileInfo{Name: "thing", Type: model.TypeDir, Mode: 0o755, Version: model.Vector{"A": 2}}
	if err := f.apply(&want{entry: dir, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(f.root, "thing")); err != nil || !fi.IsDir() {
		t.Fatal("not a directory")
	}
	if m, _ := filepath.Glob(filepath.Join(f.root, "*conflict*")); len(m) != 0 {
		t.Fatalf("an unmodified file produced a conflict copy: %v", m)
	}
}

// A directory turning into a file used to retry forever (rename onto a directory fails).
func TestDirectoryReplacedByFileConverges(t *testing.T) {
	f := newTestFolder(t)
	f.store.Put(model.FileInfo{Name: "d", Type: model.TypeDir, Mode: 0o755, Version: model.Vector{"A": 1}})
	os.MkdirAll(filepath.Join(f.root, "d"), 0o755)
	empty := fileEntry(t, "d", "", model.Vector{"A": 2}) // a zero-length file needs no download
	if err := f.apply(&want{entry: empty, sources: map[string]bool{}}, nil); err != nil {
		t.Fatalf("empty directory -> file: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(f.root, "d")); err != nil || fi.IsDir() {
		t.Fatal("still a directory")
	}

	// A directory that is not empty is preserved as a conflict copy, not lost.
	f.store.Put(model.FileInfo{Name: "e", Type: model.TypeDir, Mode: 0o755, Version: model.Vector{"A": 1}})
	os.MkdirAll(filepath.Join(f.root, "e"), 0o755)
	os.WriteFile(filepath.Join(f.root, "e", "keep.txt"), []byte("precious"), 0o644)
	if err := f.apply(&want{entry: fileEntry(t, "e", "", model.Vector{"A": 2}), sources: map[string]bool{}}, nil); err != nil {
		t.Fatalf("non-empty directory -> file: %v", err)
	}
	m, _ := filepath.Glob(filepath.Join(f.root, "e.sync-conflict-*"))
	if len(m) != 1 {
		t.Fatalf("the directory's content must be kept as a conflict copy, found %v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(m[0], "keep.txt")); string(b) != "precious" {
		t.Fatal("content lost")
	}
}

func TestConflictCopiesNeverOverwriteEachOther(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		n := uniqueConflictName(root, "a.txt", "ABCDEF", now)
		if seen[n] {
			t.Fatalf("name %s reused", n)
		}
		seen[n] = true
		os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644)
	}
}

// A timestamp rounded by the file system (exFAT, SMB) must not make an untouched
// file look edited: that would turn a normal update into a conflict copy.
func TestRoundedTimestampDoesNotLookLikeAnEdit(t *testing.T) {
	f := newTestFolder(t)
	e := writeIndexed(t, f, "r.txt", "content", model.Vector{"A": 1})
	e.ModTime += int64(time.Second) // the stored time is 1 s off the file's
	ds := f.inspect(filepath.Join(f.root, "r.txt"), e, true)
	if !ds.untouched {
		t.Fatal("a rounded timestamp with identical content was treated as a local edit")
	}
	// ...but a real edit with the same size inside the slack window is still caught.
	os.WriteFile(filepath.Join(f.root, "r.txt"), []byte("CONTENT"), 0o644)
	if f.inspect(filepath.Join(f.root, "r.txt"), e, true).untouched {
		t.Fatal("an edit of the same size within the timestamp slack was missed: it would be overwritten")
	}
}

func TestWindowsNameRules(t *testing.T) {
	bad := []string{"a:b", "file.txt:stream", "a<b", "a>b", `a"b`, "a|b", "a?b", "a*b", "trailing.", "trailing ", "CON", "con", "NUL.txt", "Com1", "lpt9.log",
		"bell\x07", "x\x1f", strings.Repeat("a", 256), ""}
	for _, n := range bad {
		if !windowsNameProblem(n) {
			t.Errorf("%q accepted", n)
		}
	}
	good := []string{"a.txt", "report (final).docx", "été.txt", ".hidden", "console.txt", "com10", "a-b_c", "x.tar.gz", "COM"}
	for _, n := range good {
		if windowsNameProblem(n) {
			t.Errorf("%q rejected", n)
		}
	}
	old := onWindows
	onWindows = true
	defer func() { onWindows = old }()
	if validName("docs/CON") || validName("a:b") || validName(".P2PSYNC/tmp/x") || validName(".p2psync") {
		t.Error("validName accepted a name Windows cannot hold or the metadata folder")
	}
	if !validName("docs/readme.md") {
		t.Error("validName rejected an ordinary name")
	}
}

func TestDamagedIndexIsRecoveredNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index-t.json")

	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Put(model.FileInfo{Name: "a", Version: model.Vector{"A": 1}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Put(model.FileInfo{Name: "b", Version: model.Vector{"A": 1}})
	if err := s.Save(); err != nil { // the previous good copy is now kept as .bak
		t.Fatal(err)
	}

	// Power cut: the main file is truncated.
	os.WriteFile(path, []byte(`{"seq": 2, "files": {"a": {"na`), 0o600)
	s2, err := openStore(path)
	if err != nil {
		t.Fatalf("a damaged index must not be fatal: %v", err)
	}
	if s2.Recovered == "" {
		t.Fatal("recovery was not reported")
	}
	if _, ok := s2.Get("a"); !ok {
		t.Fatal("the previous good copy was not used")
	}

	// Both damaged: start empty, keep the evidence.
	os.WriteFile(path, []byte("garbage"), 0o600)
	os.WriteFile(path+".bak", []byte("garbage"), 0o600)
	s3, err := openStore(path)
	if err != nil || len(s3.All()) != 0 || s3.Recovered == "" {
		t.Fatalf("expected an empty, flagged index: %v %v %q", err, len(s3.All()), s3.Recovered)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatal("the damaged file should be kept for inspection")
	}
}

func TestCaseOnlyTombstoneDoesNotDeleteTheRenamedFile(t *testing.T) {
	old := caseInsensitive
	caseInsensitive = true
	defer func() { caseInsensitive = old }()
	f := newTestFolder(t)
	// Both spellings are live in the index (the rename was announced as new file + tombstone).
	writeIndexed(t, f, "readme.md", "same file", model.Vector{"A": 1})
	writeIndexed(t, f, "README.md", "same file", model.Vector{"A": 2})
	tomb := model.FileInfo{Name: "readme.md", Type: model.TypeFile, Deleted: true, Version: model.Vector{"A": 3}}
	if err := f.apply(&want{entry: tomb, sources: map[string]bool{}}, nil); err != nil {
		t.Fatal(err)
	}
	// On NTFS "readme.md" and "README.md" are one file: removing the path would have deleted it.
	if read(t, f, "readme.md") != "same file" {
		t.Fatal("the tombstone of the old spelling removed the renamed file")
	}
	if got, _ := f.store.Get("readme.md"); !got.Deleted {
		t.Fatal("the tombstone must still be recorded in the index")
	}
}

// Runs only where the file system ignores case (Windows, macOS): renaming
// a.txt to A.txt must retire the old spelling in the index.
func TestCaseOnlyRenameRetiresTheOldSpelling(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("needs a case-insensitive file system")
	}
	f := newTestFolder(t)
	f.ctx = context.Background()
	close(f.scanned)
	os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("hello"), 0o644)
	f.scan("")
	if err := os.Rename(filepath.Join(f.root, "a.txt"), filepath.Join(f.root, "A.txt")); err != nil {
		t.Fatal(err)
	}
	f.scan("")
	old, _ := f.store.Get("a.txt")
	renamed, ok := f.store.Get("A.txt")
	if !old.Deleted || !ok || renamed.Deleted {
		t.Fatalf("old spelling should be a tombstone and the new one live: old=%+v new=%+v", old, renamed)
	}
}
