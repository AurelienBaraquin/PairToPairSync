package folder

import (
	"sort"
	"strings"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// HandleIndex compares what a peer announces with what we have and records
// what must be downloaded. It never touches the disk itself.
func (f *Folder) HandleIndex(p *node.Peer, idx protocol.Index) {
	if !f.waitScanned() || f.isBroken() {
		return
	}
	ig := f.ignore.Load()
	var changed []model.FileInfo
	needed := false

	entries := idx.Entries
	if f.isRelay(p) {
		// From a relay we accept sealed entries only, and only if they authenticate.
		entries = nil
		if f.sealer == nil {
			return
		}
		for _, se := range idx.Sealed {
			fi, err := f.unseal(se)
			if err != nil {
				f.log.Warn("ignoring entry from relay", "relay", p.Name(), "err", err)
				continue
			}
			entries = append(entries, fi)
		}
	}

	f.mu.Lock()
	for _, e := range entries {
		if !validName(e.Name) || ig.Ignored(e.Name, e.Type == model.TypeDir) || e.Version == nil {
			continue
		}
		local, have := f.store.Get(e.Name)
		switch {
		case !have:
			if e.Deleted {
				// Nothing to delete here; remember the tombstone so that we can
				// confirm it to relays and do not resurrect the file later.
				changed = append(changed, f.store.Put(e))
				continue
			}
			f.addWant(e, p.ID(), false)
			needed = true
		default:
			switch local.Version.Compare(e.Version) {
			case model.Equal, model.Greater:
				// we are up to date, or ahead
			case model.Lesser:
				if e.SameContent(local) {
					local.Version = e.Version.Clone()
					changed = append(changed, f.store.Put(local))
				} else {
					f.addWant(e, p.ID(), false)
					needed = true
				}
			case model.Concurrent:
				switch {
				case e.SameContent(local):
					// Same data edited/created independently on both sides: merge
					// the versions, no transfer and no conflict copy.
					local.Version = local.Version.Merge(e.Version)
					changed = append(changed, f.store.Put(local))
				case e.WinsOver(local):
					f.addWant(e, p.ID(), !local.Deleted)
					needed = true
				}
				// else: our version wins; the peer will fetch it and keep its own as a conflict copy.
			}
		}
	}
	f.mu.Unlock()

	f.broadcast(changed)
	if needed {
		f.kickPull()
	}
}

// addWant records a download; callers hold f.mu.
func (f *Folder) addWant(e model.FileInfo, from string, conflict bool) {
	if cur, ok := f.wanted[e.Name]; ok {
		switch cur.entry.Version.Compare(e.Version) {
		case model.Equal:
			cur.sources[from] = true
			return
		case model.Greater:
			return
		}
	}
	f.wanted[e.Name] = &want{entry: e, sources: map[string]bool{from: true}, conflict: conflict}
}

func (f *Folder) kickPull() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// pullOrder sorts work so parents exist before children and directories are
// removed only after their content.
func pullOrder(ws []*want) {
	depth := func(w *want) int { return strings.Count(w.entry.Name, "/") }
	rank := func(w *want) int {
		switch {
		case w.entry.Deleted:
			return 2
		case w.entry.Type == model.TypeDir:
			return 0
		default:
			return 1
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		ri, rj := rank(ws[i]), rank(ws[j])
		if ri != rj {
			return ri < rj
		}
		if ri == 2 {
			return depth(ws[i]) > depth(ws[j]) // deepest first
		}
		return depth(ws[i]) < depth(ws[j])
	})
}
