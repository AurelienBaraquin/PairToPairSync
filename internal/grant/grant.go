// Package grant lets an administrator device vouch for the other devices of the
// network, so that a new device only has to connect to one admin instead of to
// every device one by one.
//
// A grant is a statement signed with an admin's device key: "device S takes part
// in these folders (as a full copy or as a relay), under this name". Any device
// can check it with nothing but the issuer's ID (which is its public key); no
// server and no certificate authority is involved. Grants travel from device to
// device inside the already authenticated and encrypted sessions.
//
// What an admin can and cannot do:
//   - it can introduce devices to the folders it takes part in, and revoke them;
//   - it can make another device an admin;
//   - it cannot reach a folder it is not a member of, and a relay-only admin can
//     never introduce a full copy (which would hand out the folder secret and
//     defeat the blind relay): that rule is enforced by the receiver, see
//     internal/app/membership.go.
//
// Whom a device trusts as admin is decided by its owner: the admin it paired
// with is pinned explicitly. Further admins are those a trusted admin promoted.
package grant

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
)

const (
	Version     = 1
	maxBody     = 16 << 10
	maxFolders  = 64
	maxSubjects = 512
	// A clock that is wrong by more than this is not an honest clock: refusing such a
	// grant keeps a mistaken date from silencing every later change of that device.
	maxFuture = 7 * 24 * time.Hour
	domain    = "p2psync/grant/v1\n"
)

// Share is one folder of a grant.
type Share struct {
	ID    string `json:"id"`
	Relay bool   `json:"relay,omitempty"` // the subject is a temporary relay for this folder
}

// Grant is what an admin says about one device. The highest Seq wins.
type Grant struct {
	V       int     `json:"v"`
	Issuer  string  `json:"iss"`
	Subject string  `json:"sub"`
	Name    string  `json:"name,omitempty"`
	Admin   bool    `json:"admin,omitempty"`
	Folders []Share `json:"folders"`
	Seq     int64   `json:"seq"` // unix nanoseconds when it was issued
	Revoked bool    `json:"revoked,omitempty"`
}

// Signed is a grant with its signature. Body is the exact signed JSON, kept as
// bytes so that nothing depends on how a later version would re-encode it.
type Signed struct {
	Body []byte `json:"b"`
	Sig  []byte `json:"s"`
}

// Parse validates a body and returns the grant.
func parse(body []byte) (Grant, error) {
	if len(body) == 0 || len(body) > maxBody {
		return Grant{}, errors.New("grant: bad size")
	}
	var g Grant
	if err := json.Unmarshal(body, &g); err != nil {
		return Grant{}, fmt.Errorf("grant: %w", err)
	}
	return g, g.validate()
}

func (g *Grant) validate() error {
	if g.V != Version {
		return fmt.Errorf("grant: unsupported version %d", g.V)
	}
	var err error
	if g.Issuer, err = identity.Normalize(g.Issuer); err != nil {
		return err
	}
	if g.Subject, err = identity.Normalize(g.Subject); err != nil {
		return err
	}
	if g.Seq <= 0 {
		return errors.New("grant: no sequence number")
	}
	g.Name = config.Clean(g.Name)
	if len(g.Folders) > maxFolders {
		return errors.New("grant: too many folders")
	}
	if g.Revoked && (len(g.Folders) > 0 || g.Admin) {
		return errors.New("grant: a revocation carries no rights")
	}
	seen := map[string]bool{}
	for _, f := range g.Folders {
		if !config.ValidFolderID(f.ID) || seen[f.ID] {
			return errors.New("grant: invalid or repeated folder")
		}
		seen[f.ID] = true
	}
	return nil
}

func (g Grant) newer(o Grant) bool {
	if g.Seq != o.Seq {
		return g.Seq > o.Seq
	}
	return g.Issuer > o.Issuer
}

// SameRights reports whether two grants give the subject the same rights.
func (g Grant) SameRights(o Grant) bool {
	return g.Subject == o.Subject && g.Admin == o.Admin && g.Revoked == o.Revoked && g.Name == o.Name &&
		slices.Equal(g.Folders, o.Folders)
}

// Has returns the share for folder id.
func (g Grant) Has(id string) (Share, bool) {
	for _, f := range g.Folders {
		if f.ID == id {
			return f, true
		}
	}
	return Share{}, false
}

func payload(body []byte) []byte { return append([]byte(domain), body...) }

// Sign issues a grant with the issuer's key. The folder list is sorted so that the
// same rights always encode the same way.
func Sign(key crypto.PrivKey, g Grant, now time.Time) (Signed, error) {
	pid, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return Signed{}, err
	}
	g.V, g.Issuer = Version, pid.String()
	if g.Seq == 0 {
		g.Seq = now.UnixNano()
	}
	if g.Folders == nil {
		g.Folders = []Share{}
	}
	sort.Slice(g.Folders, func(i, j int) bool { return g.Folders[i].ID < g.Folders[j].ID })
	if err := g.validate(); err != nil {
		return Signed{}, err
	}
	body, err := json.Marshal(g)
	if err != nil {
		return Signed{}, err
	}
	sig, err := key.Sign(payload(body))
	if err != nil {
		return Signed{}, err
	}
	return Signed{Body: body, Sig: sig}, nil
}

// Verify checks the signature against the issuer named in the grant (whose ID is
// its public key) and returns the grant. It does not decide whether the issuer is
// trusted: that is the receiver's business.
func Verify(s Signed) (Grant, error) {
	g, err := parse(s.Body)
	if err != nil {
		return Grant{}, err
	}
	pid, err := peer.Decode(g.Issuer)
	if err != nil {
		return Grant{}, err
	}
	pub, err := pid.ExtractPublicKey()
	if err != nil {
		return Grant{}, err
	}
	ok, err := pub.Verify(payload(s.Body), s.Sig)
	if err != nil || !ok {
		return Grant{}, errors.New("grant: bad signature")
	}
	return g, nil
}

// ---- the set of grants a device holds ------------------------------------------------

// A device may also describe itself (Subject == Issuer): which folders it takes
// part in and in which role, so that another device can tell whether it is a full
// member or a blind relay of a folder it does not have yet. That statement is kept
// apart from what an admin says about the device (it must not replace it) and
// never confers anything by itself.
func key(g Grant) string {
	if g.Issuer == g.Subject {
		return "self/" + g.Subject
	}
	return g.Subject
}

type entry struct {
	Signed
	g Grant
}

// Set holds the newest grant known for each device.
type Set struct {
	mu   sync.Mutex
	path string
	m    map[string]entry // by key()
}

// Load reads the set stored at path. A damaged file is set aside and the set
// starts empty: grants come back from the other devices, and losing them never
// removes anything that is already configured.
func Load(path string) *Set {
	s := &Set{path: path, m: map[string]entry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var list []Signed
	if err := json.Unmarshal(b, &list); err != nil {
		_ = fsutil.Rename(path, fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix()))
		return s
	}
	for _, sg := range list {
		if g, err := Verify(sg); err == nil {
			s.keep(sg, g)
		}
	}
	return s
}

func (s *Set) keep(sg Signed, g Grant) bool {
	k := key(g)
	if cur, ok := s.m[k]; ok && !g.newer(cur.g) {
		return false
	}
	if _, ok := s.m[k]; !ok && len(s.m) >= maxSubjects {
		s.evict()
		if len(s.m) >= maxSubjects {
			return false
		}
	}
	s.m[k] = entry{sg, g}
	return true
}

// evict drops the oldest revocation to make room.
func (s *Set) evict() {
	var oldest string
	for sub, e := range s.m {
		if e.g.Revoked && (oldest == "" || e.g.Seq < s.m[oldest].g.Seq) {
			oldest = sub
		}
	}
	if oldest != "" {
		delete(s.m, oldest)
	}
}

// Trusted returns the admins this device obeys: those it pinned (its owner's
// choice, including itself when it is an admin) and, transitively, those that a
// trusted admin promoted with a grant.
func (s *Set) Trusted(pinned map[string]bool) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trustedLocked(pinned)
}

func (s *Set) trustedLocked(pinned map[string]bool) map[string]bool {
	t := map[string]bool{}
	for id, ok := range pinned {
		if ok {
			t[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, e := range s.m {
			sub := e.g.Subject
			if !t[sub] && e.g.Issuer != sub && e.g.Admin && !e.g.Revoked && t[e.g.Issuer] {
				t[sub], changed = true, true
			}
		}
	}
	return t
}

// ErrUntrusted is returned for a grant whose issuer is not an admin we trust.
var ErrUntrusted = errors.New("grant: issuer is not a trusted admin")

// Offer adds a grant received from another device. It reports whether the set
// changed. Invalid, forged, untrusted or outdated grants are refused.
func (s *Set) Offer(sg Signed, pinned map[string]bool, now time.Time) (Grant, bool, error) {
	g, err := Verify(sg)
	if err != nil {
		return Grant{}, false, err
	}
	if time.Unix(0, g.Seq).After(now.Add(maxFuture)) {
		return Grant{}, false, errors.New("grant: dated in the future")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.trustedLocked(pinned)[g.Issuer] {
		return g, false, ErrUntrusted
	}
	return g, s.keep(sg, g), nil
}

// Issue signs and stores a grant made by this device (an admin).
func (s *Set) Issue(key_ crypto.PrivKey, g Grant, now time.Time) (Signed, Grant, error) {
	if pid, err := peer.IDFromPrivateKey(key_); err == nil {
		g.Issuer = pid.String()
	}
	if g.Seq == 0 {
		// Strictly newer than the grant it replaces, even if the clock went back.
		g.Seq = now.UnixNano()
		s.mu.Lock()
		if cur, ok := s.m[key(g)]; ok && cur.g.Seq >= g.Seq {
			g.Seq = cur.g.Seq + 1
		}
		s.mu.Unlock()
	}
	sg, err := Sign(key_, g, now)
	if err != nil {
		return Signed{}, Grant{}, err
	}
	parsed, err := Verify(sg)
	if err != nil {
		return Signed{}, Grant{}, err
	}
	s.mu.Lock()
	kept := s.keep(sg, parsed)
	s.mu.Unlock()
	if !kept {
		return Signed{}, Grant{}, errors.New("grant: too many devices in the network")
	}
	return sg, parsed, nil
}

// Get returns the newest grant an admin made for a device.
func (s *Set) Get(subject string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[subject]
	return e.g, ok
}

// GetSelf returns the newest description a device gave of itself.
func (s *Set) GetSelf(subject string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m["self/"+subject]
	return e.g, ok
}

// Entry is a stored grant together with its parsed form.
type Entry struct {
	Signed Signed
	Grant  Grant
}

// All returns every stored grant, oldest subject first (stable order).
func (s *Set) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, Entry{e.Signed, e.g})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grant.Subject < out[j].Grant.Subject })
	return out
}

// Save writes the set atomically.
func (s *Set) Save() error {
	s.mu.Lock()
	list := make([]Signed, 0, len(s.m))
	for _, e := range s.m {
		list = append(list, e.Signed)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return bytes.Compare(list[i].Body, list[j].Body) < 0 })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, b, 0o600)
}

// randomID returns a short random identifier (used for pending requests).
func RandomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
