package relay

import (
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
)

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
	tmp := p + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
		for _, h := range pd.Entry.Blocks {
			keep[hex.EncodeToString(h)] = true
		}
	}
	root := filepath.Join(r.dir, "blocks")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !keep[d.Name()] {
			_ = os.Remove(p)
		}
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
