package folder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

type state struct {
	Seq       int64                     `json:"seq"`
	Files     map[string]model.FileInfo `json:"files"`
	RelaySent map[string]int64          `json:"relay_sent,omitempty"` // highest local Seq acknowledged by each relay
}

// Store is the persistent index of a folder: what this device currently has.
type Store struct {
	mu    sync.RWMutex
	path  string
	st    state
	dirty bool
	// Recovered describes a repair done when the index was opened ("" if none).
	Recovered string
}

// openStore loads the index. If the file is unreadable (a power cut on a system
// without write ordering can leave it empty or truncated) it falls back to the
// previous good copy, and as a last resort starts empty: the next scan rebuilds
// it from the disk, and identical files merge with the other devices' versions
// without conflicts. Refusing to start would take every folder down.
func openStore(path string) (*Store, error) {
	s := &Store{path: path}
	s.reset()
	for _, candidate := range []string{path, path + ".bak"} {
		b, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			var st state
			if err = json.Unmarshal(b, &st); err == nil {
				s.st = st
				if s.st.Files == nil {
					s.st.Files = map[string]model.FileInfo{}
				}
				if s.st.RelaySent == nil {
					s.st.RelaySent = map[string]int64{}
				}
				if candidate != path {
					s.Recovered = "the index was damaged; restored the previous copy"
					s.dirty = true
				}
				return s, nil
			}
		}
		s.Recovered = "the index was unreadable (" + err.Error() + "); it will be rebuilt from the folder"
		_ = os.Rename(candidate, candidate+".corrupt")
	}
	return s, nil
}

func (s *Store) reset() {
	s.st = state{Files: map[string]model.FileInfo{}, RelaySent: map[string]int64{}}
}

func (s *Store) Get(name string) (model.FileInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fi, ok := s.st.Files[name]
	return fi, ok
}

// Put records a local change and stamps it with a fresh sequence number.
func (s *Store) Put(fi model.FileInfo) model.FileInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Seq++
	fi.Seq = s.st.Seq
	s.st.Files[fi.Name] = fi
	s.dirty = true
	return fi
}

// Update changes metadata without announcing a new change (keeps Seq).
func (s *Store) Update(fi model.FileInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi.Seq = s.st.Files[fi.Name].Seq
	s.st.Files[fi.Name] = fi
	s.dirty = true
}

func (s *Store) Seq() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Seq
}

// All returns every entry, sorted by name.
func (s *Store) All() []model.FileInfo {
	return s.filter(func(model.FileInfo) bool { return true })
}

// Since returns the entries changed after seq, in sequence order.
func (s *Store) Since(seq int64) []model.FileInfo {
	out := s.filter(func(f model.FileInfo) bool { return f.Seq > seq })
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Under returns the entries equal to or below the given path ("" = all).
func (s *Store) Under(sub string) []model.FileInfo {
	return s.filter(func(f model.FileInfo) bool {
		return sub == "" || f.Name == sub || strings.HasPrefix(f.Name, sub+"/")
	})
}

func (s *Store) filter(keep func(model.FileInfo) bool) []model.FileInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.FileInfo, 0, len(s.st.Files))
	for _, f := range s.st.Files {
		if keep(f) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) RelaySent(dev string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.st.RelaySent[dev]
	return v, ok
}

func (s *Store) SetRelaySent(dev string, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.st.RelaySent[dev]; !ok || seq > cur {
		s.st.RelaySent[dev] = seq
		s.dirty = true
	}
}

// Save writes the index durably if it changed: flushed to disk, the previous good
// copy kept as ".bak", and the new one swapped in atomically. If writing fails the
// index stays marked as changed, so the next attempt retries.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(&s.st)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o700)
	}
	if err == nil {
		_ = fsutil.Rename(s.path, s.path+".bak") // keep the last good copy; absent the first time
		err = fsutil.WriteFileAtomic(s.path, b, 0o600)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}
