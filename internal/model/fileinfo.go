package model

import (
	"bytes"
	"crypto/sha256"
	"io"
)

// FileType distinguishes regular files from directories.
type FileType uint8

const (
	TypeFile FileType = iota
	TypeDir
)

// FileInfo is the metadata describing one entry of a synchronised folder. It is
// the unit exchanged between devices; file content travels separately, block by
// block.
type FileInfo struct {
	Name      string   `json:"name"` // slash separated, relative to the folder root
	Type      FileType `json:"type"`
	Size      int64    `json:"size"`
	ModTime   int64    `json:"mtime"` // unix nanoseconds
	Mode      uint32   `json:"mode"`
	Deleted   bool     `json:"deleted,omitempty"`
	Version   Vector   `json:"version"`
	BlockSize int      `json:"block_size,omitempty"`
	Blocks    [][]byte `json:"blocks,omitempty"` // SHA-256 of each block
	// Seq is a per-device local change counter. It is only meaningful on the
	// device that owns the index and is overwritten when an entry is received.
	Seq int64 `json:"seq,omitempty"`
}

// SameContent reports whether two entries describe identical data, ignoring
// versions and timestamps.
func (f FileInfo) SameContent(o FileInfo) bool {
	if f.Deleted != o.Deleted || f.Type != o.Type {
		return false
	}
	if f.Deleted || f.Type == TypeDir {
		return true
	}
	if f.Size != o.Size || len(f.Blocks) != len(o.Blocks) {
		return false
	}
	for i := range f.Blocks {
		if !bytes.Equal(f.Blocks[i], o.Blocks[i]) {
			return false
		}
	}
	return true
}

// WinsOver decides, deterministically and identically on every device, which of
// two concurrently modified versions is kept as the canonical one. The loser is
// preserved as a "sync-conflict" copy. A modification always beats a deletion.
func (f FileInfo) WinsOver(o FileInfo) bool {
	if f.Deleted != o.Deleted {
		return !f.Deleted
	}
	if f.ModTime != o.ModTime {
		return f.ModTime > o.ModTime
	}
	return f.Version.String() > o.Version.String()
}

// BlockRange returns the byte range covered by block i.
func (f FileInfo) BlockRange(i int) (off int64, size int) {
	off = int64(i) * int64(f.BlockSize)
	size = f.BlockSize
	if rest := f.Size - off; rest < int64(size) {
		size = int(rest)
	}
	return off, size
}

const (
	minBlockSize = 128 << 10
	maxBlockSize = 16 << 20
	targetBlocks = 2000
)

// BlockSizeFor picks a block size that keeps the block list of large files
// reasonably short.
func BlockSizeFor(size int64) int {
	bs := minBlockSize
	for size/int64(bs) > targetBlocks && bs < maxBlockSize {
		bs <<= 1
	}
	return bs
}

// HashBlocks reads r to the end and returns the block size used, the SHA-256 of
// every block and the number of bytes read. sizeHint selects the block size.
func HashBlocks(r io.Reader, sizeHint int64) (blockSize int, blocks [][]byte, n int64, err error) {
	blockSize = BlockSizeFor(sizeHint)
	buf := make([]byte, blockSize)
	for {
		m, rerr := io.ReadFull(r, buf)
		if m > 0 {
			h := sha256.Sum256(buf[:m])
			blocks = append(blocks, h[:])
			n += int64(m)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return blockSize, blocks, n, nil
		}
		if rerr != nil {
			return 0, nil, 0, rerr
		}
	}
}

// HashBytes returns the SHA-256 of b.
func HashBytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// WireSize estimates the JSON size of an entry, to keep index messages bounded.
func (f FileInfo) WireSize() int { return 256 + len(f.Name)*2 + len(f.Blocks)*48 + len(f.Version)*24 }

// MaxIndexMessage is the target maximum size of one index message (well under the frame limit).
const MaxIndexMessage = 8 << 20
