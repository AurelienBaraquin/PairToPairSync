package folder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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
}

func openStore(path string) (*Store, error) {
	s := &Store{path: path, st: state{Files: map[string]model.FileInfo{}, RelaySent: map[string]int64{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, err
	}
	if s.st.Files == nil {
		s.st.Files = map[string]model.FileInfo{}
	}
	if s.st.RelaySent == nil {
		s.st.RelaySent = map[string]int64{}
	}
	return s, nil
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

// Save writes the index atomically if it changed.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(&s.st)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
