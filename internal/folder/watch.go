package folder

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// startWatcher registers the folder tree with the OS file notification API so
// that edits are noticed within a fraction of a second. If it cannot (limits,
// unsupported file system) the periodic rescan still keeps the folder in sync.
func (f *Folder) startWatcher() *fsnotify.Watcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		f.log.Warn("file watcher unavailable, relying on periodic scans", "err", err)
		return nil
	}
	f.watchTree(w, f.root)
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				f.onEvent(w, ev)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				f.log.Warn("file watcher error", "err", err)
				f.queueScan("") // events may have been dropped
			case <-f.ctx.Done():
				return
			}
		}
	}()
	return w
}

func (f *Folder) watchTree(w *fsnotify.Watcher, start string) {
	ig := f.ignore.Load()
	_ = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(f.root, p)
		rel = filepath.ToSlash(rel)
		if rel != "." && ig.Ignored(rel, true) {
			return fs.SkipDir
		}
		if err := w.Add(p); err != nil {
			f.log.Warn("cannot watch directory", "path", p, "err", err)
		}
		return nil
	})
}

func (f *Folder) onEvent(w *fsnotify.Watcher, ev fsnotify.Event) {
	rel, err := filepath.Rel(f.root, ev.Name)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return
	}
	if rel == IgnoreFile {
		f.queueScan("")
		return
	}
	if f.ignore.Load().Ignored(rel, false) {
		return
	}
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
			f.watchTree(w, ev.Name)
		}
	}
	f.queueScan(rel)
}

func (f *Folder) queueScan(rel string) {
	select {
	case f.dirty <- rel:
	default:
		// Queue full: fall back to a full rescan.
		select {
		case f.dirty <- "":
		default:
		}
	}
}
