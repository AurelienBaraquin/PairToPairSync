package relay

import (
	"encoding/hex"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func isTemp(name string) bool {
	return strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".part")
}

// partGrace is how long an unreferenced temporary file is left alone.
const partGrace = 10 * time.Minute

func (r *Relay) blockPath(hash []byte) string {
	h := hex.EncodeToString(hash)
	return filepath.Join(r.dir, "blocks", h[:2], h)
}

func (r *Relay) hasBlock(hash []byte) bool {
	if len(hash) != 32 {
		return false
	}
	_, err := os.Stat(r.blockPath(hash))
	return err == nil
}

func (r *Relay) writeBlock(hash, data []byte) error {
	p := r.blockPath(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	// A block is written to its final name only once complete and flushed, so a
	// crash can leave a stray ".part" (cleaned later) but never a short block.
	return fsutil.WriteFileAtomic(p, data, 0o600)
}

func (r *Relay) storeBytesLocked() int64 {
	var total int64
	_ = filepath.WalkDir(filepath.Join(r.dir, "blocks"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// gcBlocksLocked deletes every stored block that no pending change references.
// This is what makes the relay forget delivered data.
func (r *Relay) gcBlocksLocked() {
	keep := map[string]bool{}
	for _, pd := range r.pending {
		for _, h := range pd.Entry.CBlocks {
			keep[hex.EncodeToString(h)] = true
		}
	}
	root := filepath.Join(r.dir, "blocks")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if keep[d.Name()] {
			return nil
		}
		// A temporary file may belong to a download in progress (a 16 MB block over a
		// slow link takes minutes): only remove ones that have clearly been abandoned.
		if isTemp(d.Name()) {
			if info, e := d.Info(); e == nil && time.Since(info.ModTime()) < partGrace {
				return nil
			}
		}
		_ = os.Remove(p)
		return nil
	})
	// Remove now-empty shard directories.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			_ = os.Remove(filepath.Join(root, e.Name())) // fails (and is kept) if not empty
		}
	}
}
