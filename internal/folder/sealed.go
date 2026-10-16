package folder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// Everything that crosses to or from a relay goes through this file: a relay is
// never trusted with names or content.

// sealEntry builds the only description of a change a relay will ever see.
func (f *Folder) sealEntry(fi model.FileInfo) (protocol.SealedEntry, error) {
	fi.Seq = 0
	plain, err := json.Marshal(fi)
	if err != nil {
		return protocol.SealedEntry{}, err
	}
	id := f.sealer.PathID(fi.Name)
	meta, err := f.sealer.SealMeta(plain, id, fi.Version.String())
	if err != nil {
		return protocol.SealedEntry{}, err
	}
	se := protocol.SealedEntry{ID: id, Version: fi.Version, ModTime: fi.ModTime, Deleted: fi.Deleted,
		Size: fi.Size, BlockSize: fi.BlockSize, Meta: meta}
	for _, h := range fi.Blocks {
		se.CBlocks = append(se.CBlocks, f.sealer.BlockID(h))
	}
	return se, nil
}

// unseal authenticates and decrypts an entry received from a relay, and checks
// that the cleartext fields the relay uses for ordering match the encrypted
// truth (so a relay cannot alter versions, sizes or block lists).
func (f *Folder) unseal(se protocol.SealedEntry) (model.FileInfo, error) {
	plain, err := f.sealer.OpenMeta(se.Meta, se.ID, se.Version.String())
	if err != nil {
		return model.FileInfo{}, fmt.Errorf("cannot authenticate entry from relay (wrong folder secret?): %w", err)
	}
	var fi model.FileInfo
	if err := json.Unmarshal(plain, &fi); err != nil {
		return model.FileInfo{}, err
	}
	switch {
	case f.sealer.PathID(fi.Name) != se.ID:
		return fi, errors.New("path id mismatch")
	case fi.Version.String() != se.Version.String() || fi.Deleted != se.Deleted || fi.ModTime != se.ModTime:
		return fi, errors.New("relay entry inconsistent with its sealed metadata")
	case fi.Size != se.Size || fi.BlockSize != se.BlockSize || len(fi.Blocks) != len(se.CBlocks):
		return fi, errors.New("relay entry shape mismatch")
	}
	for i, h := range fi.Blocks {
		if !bytes.Equal(f.sealer.BlockID(h), se.CBlocks[i]) {
			return fi, errors.New("block id mismatch")
		}
	}
	return fi, nil
}

func (f *Folder) isRelay(p *node.Peer) bool { return f.cfg.IsRelay(p.ID()) }

// pathIDs maps opaque relay identifiers back to the names in our index. It is
// rebuilt only when the index has changed.
func (f *Folder) resolveID(id string) (string, bool) {
	seq := f.store.Seq()
	f.idMu.Lock()
	defer f.idMu.Unlock()
	if f.idMap == nil || f.idSeq != seq {
		f.idMap = make(map[string]string)
		for _, e := range f.store.All() {
			f.idMap[f.sealer.PathID(e.Name)] = e.Name
		}
		f.idSeq = seq
	}
	name, ok := f.idMap[id]
	return name, ok
}

// serveSealedBlock answers a relay asking for one encrypted block.
func (f *Folder) serveSealedBlock(r protocol.Request) ([]byte, string) {
	name, ok := f.resolveID(r.Name)
	if !ok {
		return nil, protocol.ErrNotFound
	}
	cur, ok := f.store.Get(name)
	if !ok || cur.Deleted || cur.Type != model.TypeFile {
		return nil, protocol.ErrNotFound
	}
	plain, code := f.readBlock(name, r.Offset, r.Size)
	if code != protocol.ErrNone {
		return nil, code
	}
	h := model.HashBytes(plain)
	if !bytes.Equal(f.sealer.BlockID(h), r.Hash) {
		return nil, protocol.ErrChanged // edited since it was announced
	}
	return f.sealer.SealBlock(plain, h), protocol.ErrNone
}

func (f *Folder) readBlock(name string, off int64, size int) ([]byte, string) {
	p, err := f.abs(name)
	if err != nil {
		return nil, protocol.ErrNotFound
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, protocol.ErrNotFound
	}
	defer file.Close()
	// Never allocate what the file cannot supply: the size comes from a peer.
	if fi, err := file.Stat(); err != nil || size <= 0 || off < 0 || off+int64(size) > fi.Size() {
		return nil, protocol.ErrChanged
	}
	buf := make([]byte, size)
	if n, _ := file.ReadAt(buf, off); n != size {
		return nil, protocol.ErrChanged
	}
	return buf, protocol.ErrNone
}
