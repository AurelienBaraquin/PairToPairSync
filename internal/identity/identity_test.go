package identity

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The phone's UI asks for the device ID while the first start is still creating
// the key: every caller must get the same, complete key.
func TestConcurrentFirstUseYieldsOneKey(t *testing.T) {
	dir := t.TempDir()
	ids := make(chan string, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := LoadOrCreate(dir)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("two different identities were created: %s and %s", first, id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary key file left behind")
	}
	if fi, _ := os.Stat(filepath.Join(dir, keyFile)); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("key file must be mode 0600")
	}
}

func TestNormalizeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "abc", "12D3KooWnope", "/ip4/1.2.3.4"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
