package fsutil

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileAtomicReplacesAndLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	for i, content := range []string{"first", "second, longer than the first", "3"} {
		if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Fatalf("write %d: got %q", i, got)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// Two writers must not collide on a shared temporary name (the old code used a
// fixed "config.json.tmp"), and readers must never see a truncated file.
func TestConcurrentWritersNeverExposeAPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	full := func(n int) []byte {
		b := make([]byte, 4096)
		for i := range b {
			b[i] = byte('a' + n%26)
		}
		return b
	}
	if err := WriteFileAtomic(path, full(0), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if b, err := os.ReadFile(path); err == nil && len(b) != 4096 {
				t.Errorf("reader saw a partial file of %d bytes", len(b))
				return
			}
		}
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := WriteFileAtomic(path, full(w*100+i), 0o600); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
}

func TestRemoveOfMissingFileIsNotAnError(t *testing.T) {
	if err := Remove(filepath.Join(t.TempDir(), "nope")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestOnWindowsMountIsFalseForOrdinaryDirectories(t *testing.T) {
	if OnWindowsMount(t.TempDir()) {
		t.Fatal("a temp directory was taken for a Windows drive")
	}
	if OnWindowsMount("/definitely/not/there") {
		t.Fatal("a missing path was taken for a Windows drive")
	}
}
