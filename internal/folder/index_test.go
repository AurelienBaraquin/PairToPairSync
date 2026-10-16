package folder

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

func file(size int64, bs int, blocks int) model.FileInfo {
	e := model.FileInfo{Name: "f", Type: model.TypeFile, Size: size, BlockSize: bs, Version: model.Vector{"A": 1}}
	for i := 0; i < blocks; i++ {
		e.Blocks = append(e.Blocks, bytes.Repeat([]byte{1}, 32))
	}
	return e
}

// Entries come from other devices. Nothing derived from a malformed one may
// reach code that allocates, truncates or indexes by those numbers.
func TestValidEntryRejectsForgedShapes(t *testing.T) {
	good := file(300<<10, 128<<10, 3)
	if !validEntry(good) {
		t.Fatal("a well-formed entry was rejected")
	}
	bad := map[string]model.FileInfo{
		"negative block size":     file(5, -5, 1),
		"zero block size":         file(5, 0, 1),
		"huge block size":         file(5, 1<<30, 1),
		"negative size":           file(-1, 128<<10, 0),
		"huge size, no blocks":    file(1<<50+1, 128<<10, 0),
		"size without blocks":     file(1<<40, 128<<10, 0),
		"too few blocks":          file(300<<10, 128<<10, 2),
		"too many blocks":         file(300<<10, 128<<10, 4),
		"truncated hash":          func() model.FileInfo { e := file(5, 128<<10, 1); e.Blocks[0] = []byte{1, 2}; return e }(),
		"tombstone with data":     func() model.FileInfo { e := file(5, 128<<10, 1); e.Deleted = true; return e }(),
		"directory with data":     func() model.FileInfo { e := file(5, 128<<10, 1); e.Type = model.TypeDir; return e }(),
		"unknown type":            func() model.FileInfo { e := file(0, 0, 0); e.Type = 9; return e }(),
		"enormous name":           func() model.FileInfo { e := file(0, 0, 0); e.Name = strings.Repeat("a", 5000); return e }(),
		"empty file with a block": file(0, 128<<10, 1),
	}
	for name, e := range bad {
		if validEntry(e) {
			t.Errorf("%s: accepted", name)
		}
	}
	if !validEntry(model.FileInfo{Name: "d", Type: model.TypeDir, Version: model.Vector{"A": 1}}) ||
		!validEntry(model.FileInfo{Name: "x", Deleted: true, Version: model.Vector{"A": 1}}) ||
		!validEntry(file(0, 0, 0)) {
		t.Error("directories, tombstones and empty files must be accepted")
	}
}
