package pairing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Pending is what the inviter remembers about an outstanding invitation. The
// token itself is not stored, only its hash; the file name is the hash too, so
// looking one up needs no comparison of secrets.
type Pending struct {
	Folder       string `json:"folder"`
	InviteeRelay bool   `json:"invitee_relay"`
	Exp          int64  `json:"exp"`
}

// Store keeps outstanding invitations as one file each in dir. A file is
// consumed by removing it, which the file system makes atomic: a token can
// never be used twice, even by two processes at once.
type Store struct{ dir string }

func NewStore(home string) *Store { return &Store{dir: filepath.Join(home, "invites")} }

func (s *Store) path(token string) string { return filepath.Join(s.dir, TokenKey(token)+".json") }

// Add records an invitation.
func (s *Store) Add(token string, p Pending) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := s.path(token) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(token))
}

// Consume validates a token and burns it. It returns false for an unknown,
// already used or expired token. An expired token is removed as well.
func (s *Store) Consume(token string, now time.Time) (Pending, bool) {
	if token == "" || len(token) > 64 {
		return Pending{}, false
	}
	path := s.path(token)
	b, err := os.ReadFile(path)
	if err != nil {
		return Pending{}, false
	}
	if err := os.Remove(path); err != nil {
		return Pending{}, false // someone else consumed it first
	}
	var p Pending
	if json.Unmarshal(b, &p) != nil || now.Unix() > p.Exp {
		return Pending{}, false
	}
	return p, true
}

// Active reports whether at least one unexpired invitation exists; it also
// sweeps expired ones. While it is false the node accepts no unknown device.
func (s *Store) Active(now time.Time) bool {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return false
	}
	active := false
	for _, e := range entries {
		p := filepath.Join(s.dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var pd Pending
		if json.Unmarshal(b, &pd) != nil || now.Unix() > pd.Exp {
			_ = os.Remove(p)
			continue
		}
		active = true
	}
	return active
}

// Purge cancels every outstanding invitation (used after repeated bad tokens).
func (s *Store) Purge() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(s.dir, e.Name()))
	}
}

var ErrNoDaemon = errors.New("the p2psync daemon must be running to answer the invitation (start `p2psync run`)")
