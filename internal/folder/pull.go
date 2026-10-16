package folder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

const pullWorkers = 4

func (f *Folder) pullLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.kick:
		case <-t.C:
		case <-f.ctx.Done():
			return
		}
		if !f.waitScanned() {
			return
		}
		f.pullRound()
	}
}

func (f *Folder) pullRound() {
	for {
		f.mu.Lock()
		now := time.Now()
		var todo []*want
		for _, w := range f.wanted {
			if !w.retryAt.After(now) {
				todo = append(todo, w)
			}
		}
		f.mu.Unlock()
		if len(todo) == 0 {
			f.setStatus("idle")
			return
		}
		pullOrder(todo)
		f.setStatus("syncing")

		progressed := false
		for _, w := range todo {
			if f.ctx.Err() != nil {
				return
			}
			peer := f.pickSource(w)
			if peer == nil {
				continue // nobody who has it is connected right now
			}
			err := f.safeApply(w, peer)
			f.mu.Lock()
			if f.wanted[w.entry.Name] == w {
				if err == nil {
					delete(f.wanted, w.entry.Name)
					progressed = true
				} else {
					w.failures++
					w.retryAt = time.Now().Add(min(time.Duration(w.failures)*2*time.Second, PullRetryMax))
					f.log.Warn("sync failed, will retry", "file", w.entry.Name, "err", err)
				}
			}
			f.mu.Unlock()
		}
		if !progressed {
			f.setStatus("idle")
			return
		}
	}
}

// pickSource chooses a connected peer that announced the wanted version.
func (f *Folder) pickSource(w *want) *node.Peer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var relay *node.Peer
	for id := range w.sources {
		p := f.peers[id]
		if p == nil || !peerAlive(p) {
			continue
		}
		if !f.cfg.IsRelay(id) {
			return p // prefer a full device over a relay
		}
		relay = p
	}
	return relay
}

// diskState describes the path on disk relative to what our index believes.
type diskState struct {
	exists    bool
	isDir     bool
	untouched bool // exists and matches the index entry exactly
}

// mtimeSlack is how far a stored timestamp may differ from the file's own and
// still be considered the same instant: FAT and exFAT keep 2-second resolution,
// SMB shares and some sync clients round as well.
const mtimeSlack = 2 * time.Second

func (f *Folder) inspect(abs string, cur model.FileInfo, have bool) diskState {
	fi, err := os.Lstat(abs)
	if err != nil {
		return diskState{}
	}
	ds := diskState{exists: true, isDir: fi.IsDir()}
	if have && !cur.Deleted {
		switch {
		case fi.IsDir():
			ds.untouched = cur.Type == model.TypeDir
		case fi.Mode().IsRegular() && cur.Type == model.TypeFile && cur.Size == fi.Size():
			d := time.Duration(fi.ModTime().UnixNano() - cur.ModTime)
			switch {
			case d == 0:
				ds.untouched = true
			case d > -mtimeSlack && d < mtimeSlack:
				// A rounded timestamp or a real edit within the same moment: only the
				// content can tell. (Overwriting on a guess would lose an edit.)
				ds.untouched = contentMatches(abs, cur)
			}
		}
	}
	return ds
}

// contentMatches hashes the file and compares it with the indexed blocks.
func contentMatches(abs string, cur model.FileInfo) bool {
	file, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer file.Close()
	_, blocks, _, err := model.HashBlocks(file, cur.Size)
	if err != nil || len(blocks) != len(cur.Blocks) {
		return false
	}
	for i := range blocks {
		if !bytes.Equal(blocks[i], cur.Blocks[i]) {
			return false
		}
	}
	return true
}

// inspectName is inspect, aware of case-insensitive file systems. If "a.txt" is
// being replaced by "A.txt" (a case-only rename), the file on disk matches the
// *old* entry, and must not be taken for an unknown local file.
func (f *Folder) inspectName(name, abs string, cur model.FileInfo, have bool) diskState {
	ds := f.inspect(abs, cur, have)
	if caseInsensitive && ds.exists && !ds.untouched {
		for _, sib := range f.foldSiblings(name) {
			if f.inspect(abs, sib, true).untouched {
				ds.untouched = true
				break
			}
		}
	}
	return ds
}

// foldSiblings returns live index entries whose name differs from name only by case.
func (f *Folder) foldSiblings(name string) []model.FileInfo {
	if !caseInsensitive {
		return nil
	}
	lower := strings.ToLower(name)
	var out []model.FileInfo
	for _, e := range f.store.Under("") {
		if !e.Deleted && e.Name != name && strings.ToLower(e.Name) == lower {
			out = append(out, e)
		}
	}
	return out
}

var errLocalChanged = errors.New("the local file changed while the update was being prepared")

// apply brings one wanted entry to disk.
//
// The decision made when the change was announced may be stale by now (the
// download queued behind a slow one, the source offline, a retry back-off): the
// user can have edited the file since, and the scanner recorded a newer local
// version. So it is made again here, from the index as it is *now*, and again
// just before the file is replaced.
func (f *Folder) apply(w *want, peer *node.Peer) error {
	e := w.entry
	abs, err := f.abs(e.Name)
	if err != nil {
		return err
	}
	cur, have := f.store.Get(e.Name)
	conflict := false
	if have {
		switch cur.Version.Compare(e.Version) {
		case model.Equal, model.Greater:
			return nil // we already have this, or something newer: nothing to apply
		case model.Concurrent:
			switch {
			case e.SameContent(cur):
				cur.Version = cur.Version.Merge(e.Version)
				f.broadcast([]model.FileInfo{f.store.Put(cur)})
				return nil
			case !e.WinsOver(cur):
				return nil // our version wins; the other device will fetch it and keep its own as a copy
			}
			conflict = !cur.Deleted // the local version loses: it must be preserved
		}
	}
	w.conflict = conflict
	ds := f.inspectName(e.Name, abs, cur, have)

	switch {
	case e.Deleted:
		return f.applyDelete(e, abs, ds)
	case e.Type == model.TypeDir:
		return f.applyDir(e, abs, ds)
	default:
		return f.applyFile(w, peer, abs, cur, have, ds)
	}
}

func (f *Folder) applyDelete(e model.FileInfo, abs string, ds diskState) error {
	if ds.exists && !ds.untouched {
		// The local copy was modified (or is new) after the last scan: a
		// modification always beats a deletion. Drop the remote deletion; the
		// next scan turns the local edit into a version that supersedes it.
		f.queueScan(e.Name)
		return nil
	}
	// On a case-insensitive file system "a.txt" and "A.txt" are one file. If the
	// index holds the file under another case, this tombstone is the *old half of a
	// rename*: removing the path would delete the renamed file.
	if ds.exists && len(f.foldSiblings(e.Name)) > 0 {
		f.broadcast([]model.FileInfo{f.store.Put(e)})
		return nil
	}
	if ds.exists {
		if err := fsutil.Remove(abs); err != nil {
			return err // typically a directory that is not empty yet; retried
		}
	}
	f.broadcast([]model.FileInfo{f.store.Put(e)})
	return nil
}

func (f *Folder) applyDir(e model.FileInfo, abs string, ds diskState) error {
	if ds.exists && !ds.isDir {
		if ds.untouched {
			// An unmodified file becoming a directory is a plain replacement, not a conflict.
			if err := fsutil.Remove(abs); err != nil {
				return err
			}
		} else if err := f.moveAside(e.Name, abs); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	f.broadcast([]model.FileInfo{f.store.Put(e)})
	return nil
}

func (f *Folder) applyFile(w *want, peer *node.Peer, abs string, cur model.FileInfo, have bool, ds diskState) error {
	e := w.entry
	tmp, err := f.download(e, peer, abs, cur, have, ds)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// The download took time. Look again, at the index and at the disk, right
	// before swapping: a local edit recorded meanwhile must be decided afresh (the
	// next attempt will), and one not yet recorded must be kept.
	if latest, has := f.store.Get(e.Name); has != have || (has && latest.Version.Compare(cur.Version) != model.Equal) {
		return errLocalChanged
	}
	ds = f.inspectName(e.Name, abs, cur, have)
	if ds.exists && ds.isDir {
		// A directory is being replaced by a file: an empty one is simply removed.
		if err := os.Remove(abs); err != nil {
			if err := f.moveAside(e.Name, abs); err != nil {
				return err
			}
		}
	} else if ds.exists && (!ds.untouched || w.conflict) {
		if err := f.moveAside(e.Name, abs); err != nil {
			return err
		}
	}
	if err := fsutil.Rename(tmp, abs); err != nil {
		// Windows refuses to replace a read-only file.
		fsutil.ClearReadOnly(abs)
		if err = fsutil.Rename(tmp, abs); err != nil {
			return err
		}
	}
	f.broadcast([]model.FileInfo{f.store.Put(e)})
	return nil
}

// moveAside renames the file at abs to a sync-conflict copy and asks for it to be indexed.
func (f *Folder) moveAside(name, abs string) error {
	cname := uniqueConflictName(f.root, name, f.short, time.Now())
	dst := filepath.Join(f.root, filepath.FromSlash(cname))
	if err := fsutil.Rename(abs, dst); err != nil {
		return err
	}
	f.log.Info("conflict: kept local version as a copy", "file", name, "copy", cname)
	f.queueScan(cname)
	f.queueScan(name)
	return nil
}

type blockLoc struct {
	off  int64
	size int
}

// partName names the partial download of one specific version of a file, so an
// interrupted transfer can be resumed later, and a newer version never mixes
// with it.
func partName(e model.FileInfo) string {
	sum := sha256.Sum256([]byte(e.Name + "|" + e.Version.String()))
	return hex.EncodeToString(sum[:16]) + ".part"
}

// download builds the new content in a temporary file inside the folder's
// metadata directory (same file system, so the final rename is atomic).
// Blocks that already exist in the local version are copied, not downloaded, and
// blocks already received by an earlier, interrupted attempt are verified and
// kept: on an unreliable link a big file converges instead of restarting.
func (f *Folder) download(e model.FileInfo, peer *node.Peer, abs string, cur model.FileInfo, have bool, ds diskState) (string, error) {
	tmp := filepath.Join(f.root, MetaDir, "tmp", partName(e))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	var resumeSize int64
	if fi, err := out.Stat(); err == nil {
		resumeSize = fi.Size()
	}
	ok := false
	defer func() {
		out.Close()
		_ = ok // the partial file is deliberately kept on failure
	}()

	// Blocks we can reuse from the current local file.
	reuse := map[string]blockLoc{}
	var old *os.File
	if have && !cur.Deleted && cur.Type == model.TypeFile && ds.exists && ds.untouched && len(cur.Blocks) > 0 {
		if old, err = os.Open(abs); err == nil {
			defer old.Close()
			for i, h := range cur.Blocks {
				off, size := cur.BlockRange(i)
				if _, dup := reuse[string(h)]; !dup {
					reuse[string(h)] = blockLoc{off, size}
				}
			}
		}
	}

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }
	jobs := make(chan int)
	for i := 0; i < pullWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				off, size := e.BlockRange(i)
				if off+int64(size) <= resumeSize && partHasBlock(out, off, size, e.Blocks[i]) {
					continue // received during an earlier attempt
				}
				data, err := f.fetchBlock(ctx, e, i, size, off, peer, old, reuse)
				if err == nil {
					_, err = out.WriteAt(data, off)
				}
				if err != nil {
					fail(err)
					return
				}
			}
		}()
	}
feed:
	for i := range e.Blocks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	if f.ctx.Err() != nil {
		return "", f.ctx.Err()
	}
	if err := out.Truncate(e.Size); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	mode := os.FileMode(e.Mode&0o777) | 0o600
	if err := os.Chmod(tmp, mode); err != nil {
		return "", err
	}
	mt := time.Unix(0, e.ModTime)
	if err := os.Chtimes(tmp, mt, mt); err != nil {
		return "", err
	}
	ok = true
	return tmp, nil
}

func partHasBlock(file *os.File, off int64, size int, hash []byte) bool {
	buf := make([]byte, size)
	if n, _ := file.ReadAt(buf, off); n != size {
		return false
	}
	return string(model.HashBytes(buf)) == string(hash)
}

func (f *Folder) fetchBlock(ctx context.Context, e model.FileInfo, i, size int, off int64, peer *node.Peer, old *os.File, reuse map[string]blockLoc) ([]byte, error) {
	hash := e.Blocks[i]
	if loc, ok := reuse[string(hash)]; ok && loc.size == size && old != nil {
		buf := make([]byte, size)
		if _, err := old.ReadAt(buf, loc.off); err == nil || errors.Is(err, io.EOF) {
			if string(model.HashBytes(buf)) == string(hash) {
				return buf, nil
			}
		}
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ { // ride out brief network hiccups
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		data, err := f.requestBlock(ctx, e, size, off, hash, peer)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("block %d of %s: %w", i, e.Name, lastErr)
}

func (f *Folder) requestBlock(ctx context.Context, e model.FileInfo, size int, off int64, hash []byte, peer *node.Peer) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !f.isRelay(peer) {
		data, err := peer.Request(rctx, protocol.Request{Folder: f.cfg.ID, Name: e.Name, Offset: off, Size: size, Hash: hash})
		f.recv.Add(int64(len(data)))
		return data, err
	}
	// Through a relay the block travels encrypted and is authenticated here.
	body, err := peer.Request(rctx, protocol.Request{Folder: f.cfg.ID, Name: f.sealer.PathID(e.Name),
		Offset: off, Size: size, Hash: f.sealer.BlockID(hash), Sealed: true})
	if err != nil {
		return nil, err
	}
	f.recv.Add(int64(len(body)))
	return f.sealer.OpenBlock(body, hash)
}

// peerAlive reports whether a session is still open; a table entry can briefly outlive its connection.
func peerAlive(p *node.Peer) bool {
	select {
	case <-p.Done():
		return false
	default:
		return true
	}
}

// safeApply applies one entry; a panic (a bug triggered by unexpected data) fails
// that entry, with a log, instead of taking the whole daemon down.
func (f *Folder) safeApply(w *want, peer *node.Peer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			f.log.Error("recovered from a panic while applying a change", "file", w.entry.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return f.apply(w, peer)
}
