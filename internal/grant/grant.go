// Package grant lets the full devices of a network introduce the devices they were
// paired with to each other, so that a new device only has to be paired with one
// device instead of with every one of them.
//
// An introduction (a grant) is a statement signed with a device's key: "device S
// takes part in these folders (as a full copy or as a relay), under this name".
// Anyone can check it with nothing but the issuer's ID (which is its public key); no
// server and no certificate authority is involved. Grants travel from device to
// device inside the already authenticated and encrypted sessions.
//
// Who may introduce whom:
//   - only full devices introduce: a relay never has the folder secret, and a relay
//     that could introduce a full copy would defeat that blindness (the receiver
//     enforces it, see internal/app/membership.go);
//   - a device only introduces others to the folders it is itself a member of;
//   - a device trusts the full devices its owner paired it with, and those that a
//     trusted device introduced as full devices in turn.
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
	maxSubjects = 1024
	// A clock that is wrong by more than this is not an honest clock.
	maxFuture = 24 * time.Hour
	domain    = "p2psync/grant/v1\n"
)

// Share is one folder of a grant.
type Share struct {
	ID    string `json:"id"`
	Relay bool   `json:"relay,omitempty"` // the subject is a temporary relay for this folder
}

// Grant is what one device says about another. The highest Seq of each issuer wins.
type Grant struct {
	V       int    `json:"v"`
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Name    string `json:"name,omitempty"`
	// Introducer: the subject is a full device that may introduce others in turn.
	Introducer bool    `json:"intro,omitempty"`
	Folders    []Share `json:"folders"`
	Seq        int64   `json:"seq"` // unix nanoseconds when it was issued
	Revoked    bool    `json:"revoked,omitempty"`
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
	if g.Issuer == g.Subject {
		return errors.New("grant: a device cannot vouch for itself")
	}
	g.Name = config.Clean(g.Name)
	if len(g.Folders) > maxFolders {
		return errors.New("grant: too many folders")
	}
	if g.Revoked && (len(g.Folders) > 0 || g.Introducer) {
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

func (g Grant) hasFull() bool {
	for _, f := range g.Folders {
		if !f.Relay {
			return true
		}
	}
	return false
}

func (g Grant) newer(o Grant) bool {
	if g.Seq != o.Seq {
		return g.Seq > o.Seq
	}
	return g.Issuer > o.Issuer
}

// SameRights reports whether two grants give the subject the same rights.
func (g Grant) SameRights(o Grant) bool {
	return g.Subject == o.Subject && g.Introducer == o.Introducer && g.Revoked == o.Revoked && g.Name == o.Name &&
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

// Grants are kept per (issuer, subject): each device's statement about another stands on
// its own. What a device is granted is the union of what the trusted devices say, so
// one narrowing or withdrawing its own grant cannot erase another's, and a device
// introduced by another cannot cut off the one that introduced it.
func key(issuer, subject string) string { return issuer + "|" + subject }

type entry struct {
	Signed
	g Grant
	// What this device remembers about the grant it replaced. A device that narrows a
	// device to other folders must still be heard by the devices that knew it for the
	// old ones (see Entry.Mentions).
	prev           []string
	prevIntroducer bool
}

// stored is the on-disk form of an entry.
type stored struct {
	Signed
	Prev           []string `json:"prev,omitempty"`
	PrevIntroducer bool     `json:"prev_intro,omitempty"`
}

// Set holds the newest grant of each issuer for each device.
type Set struct {
	mu   sync.Mutex
	path string
	m    map[string]entry // by key(issuer, subject)
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
	var list []stored
	if err := json.Unmarshal(b, &list); err != nil {
		_ = fsutil.Rename(path, fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix()))
		return s
	}
	for _, st := range list {
		if g, err := Verify(st.Signed); err == nil && s.keep(st.Signed, g) {
			e := s.m[key(g.Issuer, g.Subject)]
			e.prev, e.prevIntroducer = capFolders(st.Prev), st.PrevIntroducer
			s.m[key(g.Issuer, g.Subject)] = e
		}
	}
	return s
}

func capFolders(in []string) []string {
	if len(in) > maxFolders {
		in = in[len(in)-maxFolders:]
	}
	return in
}

func (s *Set) keep(sg Signed, g Grant) bool {
	k := key(g.Issuer, g.Subject)
	cur, had := s.m[k]
	if had && !g.newer(cur.g) {
		return false
	}
	if !had && len(s.m) >= maxSubjects {
		s.evict()
		if len(s.m) >= maxSubjects {
			return false
		}
	}
	e := entry{Signed: sg, g: g}
	if had {
		e.prev, e.prevIntroducer = cur.prev, cur.prevIntroducer || cur.g.Introducer
		for _, f := range cur.g.Folders {
			if !slices.Contains(e.prev, f.ID) {
				e.prev = append(e.prev, f.ID)
			}
		}
		e.prev = capFolders(e.prev)
	}
	s.m[k] = e
	return true
}

// evict drops the oldest revocation to make room.
func (s *Set) evict() {
	var oldest string
	for k, e := range s.m {
		if e.g.Revoked && (oldest == "" || e.g.Seq < s.m[oldest].g.Seq) {
			oldest = k
		}
	}
	if oldest != "" {
		delete(s.m, oldest)
	}
}

// Trusted returns the devices this device accepts introductions from: those it was
// paired with as full devices (its owner's choice, and itself) and, transitively, the
// full devices that a trusted device introduced as introducers.
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
			// An introducer must be a full device: a relay is never trusted to introduce.
			if sub := e.g.Subject; !t[sub] && e.g.Introducer && !e.g.Revoked && e.g.hasFull() && t[e.g.Issuer] {
				t[sub], changed = true, true
			}
		}
	}
	return t
}

// ErrUntrusted is returned for a grant whose issuer is not a device we trust to introduce.
var ErrUntrusted = errors.New("grant: issuer is not a trusted introducer")

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

// Issue signs and stores a grant made by this device.
func (s *Set) Issue(priv crypto.PrivKey, g Grant, now time.Time) (Signed, Grant, error) {
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return Signed{}, Grant{}, err
	}
	g.Issuer = pid.String()
	if g.Seq == 0 {
		// Strictly newer than the grant it replaces, even if the clock went back.
		g.Seq = now.UnixNano()
		s.mu.Lock()
		if cur, ok := s.m[key(g.Issuer, g.Subject)]; ok && cur.g.Seq >= g.Seq {
			g.Seq = cur.g.Seq + 1
		}
		s.mu.Unlock()
	}
	sg, err := Sign(priv, g, now)
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

// By returns the newest grant that one device made for another.
func (s *Set) By(issuer, subject string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key(issuer, subject)]
	return e.g, ok
}

// Latest returns the most recent grant of any issuer about a device.
func (s *Set) Latest(subject string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best Grant
	found := false
	for _, e := range s.m {
		if e.g.Subject == subject && (!found || e.g.newer(best)) {
			best, found = e.g, true
		}
	}
	return best, found
}

// Entry is a stored grant together with its parsed form.
type Entry struct {
	Signed         Signed
	Grant          Grant
	Prev           []string // folders the replaced grant listed
	PrevIntroducer bool     // an earlier grant made the subject an introducer
}

// Mentions reports whether the grant concerns any of the folders: those it lists now
// and those it listed before it was narrowed.
func (e Entry) Mentions(folders map[string]bool) bool {
	for _, f := range e.Grant.Folders {
		if folders[f.ID] {
			return true
		}
	}
	for _, f := range e.Prev {
		if folders[f] {
			return true
		}
	}
	return false
}

// All returns every stored grant in a stable order.
func (s *Set) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, Entry{e.Signed, e.g, slices.Clone(e.prev), e.prevIntroducer})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Grant.Subject != out[j].Grant.Subject {
			return out[i].Grant.Subject < out[j].Grant.Subject
		}
		return out[i].Grant.Issuer < out[j].Grant.Issuer
	})
	return out
}

// Save writes the set atomically.
func (s *Set) Save() error {
	s.mu.Lock()
	list := make([]stored, 0, len(s.m))
	for _, e := range s.m {
		list = append(list, stored{e.Signed, e.prev, e.prevIntroducer})
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
