package folder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

// scanLoop performs the first full scan, then reacts to file system events and
// runs a periodic safety rescan.
func (f *Folder) scanLoop() {
	w := f.startWatcher()
	if w != nil {
		defer w.Close()
	}
	f.scan("")
	close(f.scanned)

	interval := f.cfg.ScanInterval()
	if fsutil.OnWindowsMount(f.root) && interval > 10*time.Second {
		// WSL does not deliver change notifications for files written by Windows programs.
		f.log.Warn("this folder is on a Windows drive seen from WSL: changes made by Windows programs are only noticed by the periodic scan (10 s); "+
			"keep the folder inside the Linux file system (~/...) for instant sync", "path", f.root)
		interval = 10 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	pending := map[string]bool{}
	var debounce <-chan time.Time
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-tick.C:
			f.scan("")
		case rel := <-f.dirty:
			pending[rel] = true
			if debounce == nil {
				debounce = time.After(WatchDebounce)
			}
		case <-debounce:
			debounce = nil
			for _, rel := range collapse(pending) {
				f.scan(rel)
			}
			pending = map[string]bool{}
		}
	}
}

// collapse drops paths already covered by a dirty ancestor.
func collapse(set map[string]bool) []string {
	var out []string
	for p := range set {
		covered := false
		for q := range set {
			if q != p && (q == "" || strings.HasPrefix(p, q+"/")) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// ScanNow synchronously rescans the whole folder (used by tests and the API).
func (f *Folder) ScanNow() {
	if f.waitScanned() {
		f.scan("")
	}
}

// scan walks sub (relative path, "" for everything), updates the index with
// what it finds and announces the changes.
func (f *Folder) scan(sub string) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			f.log.Error("recovered from a panic while scanning", "path", sub, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	if f.isBroken() {
		return
	}
	if _, err := os.Stat(filepath.Join(f.root, MetaDir)); err != nil {
		f.setError(err)
		return
	}
	if sub == "" {
		f.ignore.Store(loadIgnore(f.root))
	}
	ig := f.ignore.Load()
	f.setStatus("scanning")
	defer f.setStatus("idle")

	seen := map[string]bool{}
	seenFold := map[string]string{} // lower-case name -> name as found on disk (case-insensitive systems only)
	var errPaths []string
	var changed []model.FileInfo

	start := f.root
	if sub != "" {
		start = filepath.Join(f.root, filepath.FromSlash(sub))
	}
	if _, err := os.Lstat(start); err == nil {
		_ = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			rel, rerr := filepath.Rel(f.root, p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if err != nil {
				errPaths = append(errPaths, rel)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ig.Ignored(rel, d.IsDir()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil // symlinks, sockets, devices: not synchronised
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fs.SkipDir
			}
			seen[rel] = true
			if caseInsensitive {
				seenFold[strings.ToLower(rel)] = rel
			}
			fi, ok, retry := f.scanEntry(rel, p, d)
			if ok {
				changed = append(changed, fi)
			}
			if retry {
				errPaths = append(errPaths, rel)
				f.retryScan(rel)
			}
			return nil
		})
	}

	// Anything indexed under sub that is no longer on disk was deleted.
	for _, cur := range f.store.Under(sub) {
		if cur.Deleted || seen[cur.Name] || underAny(cur.Name, errPaths) || ig.Ignored(cur.Name, cur.Type == model.TypeDir) {
			continue
		}
		if now, renamed := seenFold[strings.ToLower(cur.Name)]; caseInsensitive && renamed && now != cur.Name {
			// "a.txt" became "A.txt": Lstat still finds the old spelling, but the entry
			// for it is stale. Without this, both names stay alive in the index and the
			// other devices end up with a conflict copy of every renamed file.
		} else if _, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(cur.Name))); err == nil {
			continue // exists but was not walked (e.g. symlink swapped in); leave it alone
		}
		cur.Deleted = true
		cur.Size, cur.Blocks, cur.BlockSize = 0, nil, 0
		cur.Version = cur.Version.Bump(f.short)
		cur.ModTime = time.Now().UnixNano()
		changed = append(changed, f.store.Put(cur))
	}

	if len(changed) > 0 {
		f.log.Debug("local changes", "count", len(changed))
		f.broadcast(changed)
	}
}

func underAny(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// retryScan schedules another look at rel shortly (used when a file was being
// written while we tried to index it).
func (f *Folder) retryScan(rel string) {
	go func() {
		select {
		case <-time.After(time.Second):
			f.queueScan(rel)
		case <-f.ctx.Done():
		}
	}()
}

// scanEntry indexes a single path. It returns the new entry if something
// changed, and retry=true if the file could not be read consistently (it was
// being written, or vanished) and should be looked at again shortly.
func (f *Folder) scanEntry(rel, p string, d fs.DirEntry) (fi model.FileInfo, changed, retry bool) {
	info, err := d.Info()
	if err != nil {
		return model.FileInfo{}, false, true
	}
	cur, have := f.store.Get(rel)
	mode := uint32(info.Mode().Perm())

	if d.IsDir() {
		if have && !cur.Deleted && cur.Type == model.TypeDir {
			return model.FileInfo{}, false, false
		}
		return f.store.Put(model.FileInfo{
			Name: rel, Type: model.TypeDir, Mode: mode, ModTime: info.ModTime().UnixNano(),
			Version: cur.Version.Bump(f.short),
		}), true, false
	}

	mtime := info.ModTime().UnixNano()
	if have && !cur.Deleted && cur.Type == model.TypeFile && cur.Size == info.Size() && cur.ModTime == mtime {
		if cur.Mode != mode {
			cur.Mode = mode
			f.store.Update(cur)
		}
		return model.FileInfo{}, false, false
	}

	file, err := os.Open(p)
	if err != nil {
		return model.FileInfo{}, false, true
	}
	bs, blocks, n, err := model.HashBlocks(file, info.Size())
	file.Close()
	if err != nil {
		return model.FileInfo{}, false, true
	}
	// If the file moved under our feet while hashing, skip it; it will be
	// looked at again once it is quiet.
	after, err := os.Lstat(p)
	if err != nil || after.Size() != n || after.ModTime().UnixNano() != mtime {
		return model.FileInfo{}, false, true
	}

	next := model.FileInfo{
		Name: rel, Type: model.TypeFile, Size: n, ModTime: mtime, Mode: mode,
		BlockSize: bs, Blocks: blocks,
	}
	if have && !cur.Deleted && cur.Type == model.TypeFile && cur.SameContent(next) {
		// Same data, only the timestamp differs (touch, or a file system with
		// coarser time resolution than the peer's). No new version.
		cur.ModTime, cur.Mode = mtime, mode
		f.store.Update(cur)
		return model.FileInfo{}, false, false
	}
	next.Version = cur.Version.Bump(f.short)
	return f.store.Put(next), true, false
}
