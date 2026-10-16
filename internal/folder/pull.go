package folder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
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
			err := f.apply(w, peer)
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
		if p == nil {
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
		case fi.Mode().IsRegular():
			ds.untouched = cur.Type == model.TypeFile && cur.Size == fi.Size() && cur.ModTime == fi.ModTime().UnixNano()
		}
	}
	return ds
}

// apply brings one wanted entry to disk.
func (f *Folder) apply(w *want, peer *node.Peer) error {
	e := w.entry
	abs, err := f.abs(e.Name)
	if err != nil {
		return err
	}
	cur, have := f.store.Get(e.Name)
	ds := f.inspect(abs, cur, have)

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
	if ds.exists {
		if err := os.Remove(abs); err != nil {
			return err // typically a directory that is not empty yet; retried
		}
	}
	f.broadcast([]model.FileInfo{f.store.Put(e)})
	return nil
}

func (f *Folder) applyDir(e model.FileInfo, abs string, ds diskState) error {
	if ds.exists && !ds.isDir {
		if err := f.moveAside(e.Name, abs); err != nil {
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
	defer os.Remove(tmp) // no-op after a successful rename

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// Look again right before swapping: the user may have edited the file while
	// we were downloading. Whatever is there and unknown to the index is kept.
	ds = f.inspect(abs, cur, have)
	if ds.exists && (!ds.untouched || w.conflict) {
		if err := f.moveAside(e.Name, abs); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, abs); err != nil {
		return err
	}
	f.broadcast([]model.FileInfo{f.store.Put(e)})
	return nil
}

// moveAside renames the file at abs to a sync-conflict copy and asks for it to be indexed.
func (f *Folder) moveAside(name, abs string) error {
	cname := conflictName(name, f.short, time.Now())
	dst := filepath.Join(f.root, filepath.FromSlash(cname))
	if err := os.Rename(abs, dst); err != nil {
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

// download builds the new content in a temporary file inside the folder's
// metadata directory (same file system, so the final rename is atomic).
// Blocks that already exist in the local version are copied, not downloaded.
func (f *Folder) download(e model.FileInfo, peer *node.Peer, abs string, cur model.FileInfo, have bool, ds diskState) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(f.root, MetaDir, "tmp", hex.EncodeToString(rnd[:]))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(tmp)
		}
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
	if err := ctx.Err(); err != nil && f.ctx.Err() != nil {
		return "", err
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
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	data, err := peer.Request(rctx, f.cfg.ID, e.Name, off, size, hash, false)
	if err != nil {
		return nil, fmt.Errorf("block %d of %s: %w", i, e.Name, err)
	}
	return data, nil
}
