// Package relay implements the temporary store-and-forward role of a folder.
//
// A relay (typically a phone) has no copy of the synchronised tree. It only
// holds the latest, not yet delivered, changes. As soon as every full device of
// the folder is known to hold a change, the relay deletes it. Nothing is kept
// permanently.
//
// A relay is also blind: it never receives the folder secret, so file names,
// metadata and data are ciphertext to it (see package seal). A lost or seized
// phone reveals sizes, timing and version counters, nothing else.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// Tunables, overridable in tests.
var (
	// HoldTime is how long the relay waits for connected devices to sync
	// directly before it fetches the data itself (when everybody is online it
	// is not needed at all).
	HoldTime      = 20 * time.Second
	ProbeInterval = 30 * time.Second
	TickInterval  = time.Second
)

const fetchWorkers = 4

// deliveredTTL bounds how long a delivery is remembered; deliveredMax bounds the table.
var (
	deliveredTTL = 24 * time.Hour
	deliveredMax = 20000
)

type delivery struct {
	Version model.Vector `json:"version"`
	At      time.Time    `json:"at"`
}

// Pending is one change waiting to be delivered to every full device.
type Pending struct {
	Entry protocol.SealedEntry `json:"entry"`
	Acked map[string]bool      `json:"acked"` // devices known to hold this version (or a newer one)
	Have  bool                 `json:"have"`  // all data blocks are stored here
	Since time.Time            `json:"since"`
}

type Relay struct {
	cfg  config.Folder
	self string
	dir  string
	log  *slog.Logger

	mu      sync.Mutex
	pending map[string]*Pending
	// delivered remembers, for a day, the version of each change just handed to every
	// device. Without it, a device announcing a version the relay has already
	// forgotten (its acknowledgement raced the relay's own probe) would look like a
	// new change, and the relay would download it again to wait for devices that have it.
	delivered map[string]delivery
	peers     map[string]*node.Peer
	fetching  map[string]bool
	overflow  bool // quota warning already logged

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}
}

func New(cfg config.Folder, selfID string, log *slog.Logger) *Relay {
	return &Relay{
		cfg: cfg, self: selfID, dir: filepath.Clean(cfg.Path), log: log.With("folder", cfg.ID, "role", "relay"),
		pending: map[string]*Pending{}, delivered: map[string]delivery{}, peers: map[string]*node.Peer{}, fetching: map[string]bool{},
		wake: make(chan struct{}, 1),
	}
}

func (r *Relay) ID() string { return r.cfg.ID }

func (r *Relay) Start(ctx context.Context) error {
	if r.cfg.Secret != "" {
		// Defence in depth (config validation already rejects this): a relay that
		// holds the secret could read what it is only meant to carry.
		return errors.New("a relay must not be given the folder secret")
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "blocks"), 0o700); err != nil {
		return err
	}
	pendingPath := filepath.Join(r.dir, "pending.json")
	if b, err := os.ReadFile(pendingPath); err == nil {
		if err := json.Unmarshal(b, &r.pending); err != nil || r.pending == nil {
			// A power cut can leave a truncated file. Refusing to start would take every
			// folder of the device down with it: set the damaged file aside and start
			// empty. Devices still hold their changes and will announce them again.
			quarantine := fmt.Sprintf("%s.corrupt-%d", pendingPath, time.Now().Unix())
			_ = os.Rename(pendingPath, quarantine)
			r.log.Error("pending.json was unreadable; set aside and starting empty", "err", err, "kept_as", filepath.Base(quarantine))
			r.pending = map[string]*Pending{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r.verifyBlocksLocked()
	if b, err := os.ReadFile(filepath.Join(r.dir, "delivered.json")); err == nil {
		_ = json.Unmarshal(b, &r.delivered) // best effort: a lost table only costs a redundant fetch
		if r.delivered == nil {
			r.delivered = map[string]delivery{}
		}
	}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.mu.Lock()
	r.gcBlocksLocked()
	r.mu.Unlock()
	r.wg.Add(2)
	go func() { defer r.wg.Done(); r.tickLoop() }()
	go func() { defer r.wg.Done(); r.probeLoop() }()
	return nil
}

func (r *Relay) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

func (r *Relay) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// ---- node.FolderHandler -----------------------------------------------------

func (r *Relay) PeerUp(p *node.Peer) {
	if r.cfg.IsRelay(p.ID()) {
		return // relays do not exchange with each other
	}
	r.mu.Lock()
	select {
	case <-p.Done(): // the session ended (or was replaced) before PeerUp ran: do not register it
		r.mu.Unlock()
		return
	default:
	}
	r.peers[p.ID()] = p
	out := r.announceableLocked(p.ID())
	r.mu.Unlock()
	r.send(p, out)
	r.probePeer(p)
}

func (r *Relay) PeerDown(p *node.Peer) {
	r.mu.Lock()
	if r.peers[p.ID()] == p {
		delete(r.peers, p.ID())
	}
	r.mu.Unlock()
	r.kick() // a device going offline may require us to fetch data now
}

func (r *Relay) announceableLocked(dev string) []protocol.SealedEntry {
	var out []protocol.SealedEntry
	for _, pd := range r.pending {
		if pd.Have && !pd.Acked[dev] {
			out = append(out, pd.Entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Relay) send(p *node.Peer, entries []protocol.SealedEntry) {
	for len(entries) > 0 {
		n, size := 0, 0
		for n < len(entries) && n < 500 {
			w := entries[n].WireSize()
			if n > 0 && size+w > model.MaxIndexMessage {
				break
			}
			size += w
			n++
		}
		if p.SendIndex(protocol.Index{Folder: r.cfg.ID, Sealed: entries[:n]}) != nil {
			return
		}
		entries = entries[n:]
	}
}

// announce pushes entries to every connected device that has not acknowledged them.
func (r *Relay) announce(names ...string) {
	r.mu.Lock()
	type job struct {
		p *node.Peer
		e []protocol.SealedEntry
	}
	var jobs []job
	for id, p := range r.peers {
		var es []protocol.SealedEntry
		for _, n := range names {
			if pd := r.pending[n]; pd != nil && pd.Have && !pd.Acked[id] {
				es = append(es, pd.Entry)
			}
		}
		if len(es) > 0 {
			jobs = append(jobs, job{p, es})
		}
	}
	r.mu.Unlock()
	for _, j := range jobs {
		go r.send(j.p, j.e)
	}
}

// HandleIndex receives sealed changes announced by a full device. Plaintext
// entries are refused outright: a relay has no business seeing them.
func (r *Relay) HandleIndex(p *node.Peer, idx protocol.Index) bool {
	if r.cfg.IsRelay(p.ID()) {
		return false
	}
	if len(idx.Entries) > 0 {
		r.log.Warn("refusing plaintext entries: this device only accepts encrypted ones", "from", p.Name())
	}
	announce, ok := r.ingest(p.ID(), idx.Sealed)
	r.announce(announce...)
	r.kick()
	return ok
}

// ingest records sealed changes announced by the full device `from` and returns
// the IDs that should be announced to the other devices.
//
// ok is false when the changes could not be stored durably (disk full, I/O
// error) or were refused for lack of room: the sender must then not be told
// they were received, so that it offers them again later instead of forgetting.
func (r *Relay) ingest(from string, entries []protocol.SealedEntry) (announce []string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ok = true
	for _, e := range entries {
		if !validSealed(e) {
			continue
		}
		if d, ok := r.delivered[e.ID]; ok && d.Version.Dominates(e.Version) {
			continue // already delivered to everyone: a late acknowledgement, not a new change
		}
		cur := r.pending[e.ID]
		if cur == nil && len(r.pending) >= maxPending {
			ok = false // table full: a flood of entries must not exhaust the phone
			continue
		}
		switch {
		case cur == nil:
			r.pending[e.ID] = r.newPendingLocked(e, from)
			delete(r.delivered, e.ID)
			announce = append(announce, e.ID)
		default:
			switch cur.Entry.Version.Compare(e.Version) {
			case model.Equal:
				cur.Acked[from] = true
			case model.Greater:
				announce = append(announce, e.ID) // the sender is behind: tell it
			case model.Lesser:
				r.pending[e.ID] = r.newPendingLocked(e, from)
				announce = append(announce, e.ID)
			case model.Concurrent:
				// Keep only the canonical winner; the loser's device receives the
				// winner and preserves its own file as a conflict copy.
				if e.WinsOver(cur.Entry) {
					r.pending[e.ID] = r.newPendingLocked(e, from)
				}
				announce = append(announce, e.ID)
			}
		}
	}
	// Save first, then collect garbage: after a crash between the two, an old entry
	// reappearing with its data deleted would be worse than a few orphan blocks.
	if err := r.saveLocked(); err != nil {
		ok = false
	}
	r.gcBlocksLocked()
	return announce, ok
}

// maxPending bounds the number of undelivered changes a relay will hold.
var maxPending = 50000

func (r *Relay) newPendingLocked(e protocol.SealedEntry, from string) *Pending {
	pd := &Pending{Entry: e, Acked: map[string]bool{from: true}, Since: time.Now(), Have: true}
	for _, h := range e.CBlocks {
		if !r.hasBlock(h) {
			pd.Have = false
			break
		}
	}
	return pd
}

func (r *Relay) HandleIndexAck(*node.Peer, protocol.IndexAck) {}

func (r *Relay) HandleProbe(_ *node.Peer, items []protocol.ProbeItem) []bool {
	return make([]bool, len(items))
}

// HandleRequest serves a stored (encrypted) block by its identifier.
func (r *Relay) HandleRequest(_ *node.Peer, req protocol.Request) ([]byte, string) {
	if !req.Sealed || len(req.Hash) != 32 {
		return nil, protocol.ErrNotFound
	}
	b, err := os.ReadFile(r.blockPath(req.Hash))
	if err != nil || len(b) != req.Size+protocol.SealOverhead {
		return nil, protocol.ErrNotFound
	}
	return b, protocol.ErrNone
}

// Status is what the local API reports for this folder.
type Status struct {
	ID           string   `json:"id"`
	Label        string   `json:"label,omitempty"`
	Path         string   `json:"path"`
	Role         string   `json:"role"`
	State        string   `json:"state"`
	PendingFiles int      `json:"pending_changes"`
	StoredBytes  int64    `json:"stored_bytes"`
	QuotaBytes   int64    `json:"quota_bytes"`
	Waiting      []string `json:"waiting_for_devices,omitempty"`
	Peers        []string `json:"connected_peers"`
}

func (r *Relay) Status() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{ID: r.cfg.ID, Label: r.cfg.Label, Path: r.cfg.Path, Role: config.RoleRelay, State: "idle",
		PendingFiles: len(r.pending), StoredBytes: r.storeBytesLocked(), QuotaBytes: r.cfg.Quota()}
	waiting := map[string]bool{}
	for _, pd := range r.pending {
		for _, d := range r.cfg.FullDevices() {
			if !pd.Acked[d] {
				waiting[d] = true
			}
		}
	}
	for d := range waiting {
		st.Waiting = append(st.Waiting, d)
	}
	sort.Strings(st.Waiting)
	for _, p := range r.peers {
		st.Peers = append(st.Peers, p.Name())
	}
	return st
}

// ---- delivery tracking ------------------------------------------------------

func (r *Relay) tickLoop() {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-r.wake:
		case <-r.ctx.Done():
			return
		}
		r.evaluate()
	}
}

// evaluate drops delivered or expired changes and starts fetching data for the
// ones that need to be held for an absent device.
func (r *Relay) evaluate() {
	r.mu.Lock()
	expected := r.cfg.FullDevices()
	now := time.Now()
	removed := false
	for name, pd := range r.pending {
		switch {
		case now.Sub(pd.Since) > r.cfg.Retention():
			r.log.Warn("dropping undelivered change after retention period", "change", short(name))
			delete(r.pending, name)
			removed = true
		case len(expected) > 0 && allAcked(pd, expected):
			r.log.Info("change delivered to every device, removed from relay", "change", short(name))
			r.delivered[name] = delivery{Version: pd.Entry.Version.Clone(), At: now}
			delete(r.pending, name)
			removed = true
		}
	}
	r.pruneDeliveredLocked(now)
	var toFetch []string
	for name, pd := range r.pending {
		if pd.Have || r.fetching[name] {
			continue
		}
		if r.mustHoldLocked(pd, expected, now) {
			toFetch = append(toFetch, name)
		}
	}
	for _, name := range toFetch {
		r.fetching[name] = true
		pd := r.pending[name]
		r.wg.Add(1)
		go func() { defer r.wg.Done(); r.fetch(name, pd) }()
	}
	if removed {
		_ = r.saveLocked() // then collect: see ingest
		r.gcBlocksLocked()
	}
	r.mu.Unlock()
}

func (r *Relay) pruneDeliveredLocked(now time.Time) {
	for id, d := range r.delivered {
		if now.Sub(d.At) > deliveredTTL {
			delete(r.delivered, id)
		}
	}
	for len(r.delivered) > deliveredMax { // drop the oldest
		var oldest string
		var at time.Time
		for id, d := range r.delivered {
			if oldest == "" || d.At.Before(at) {
				oldest, at = id, d.At
			}
		}
		delete(r.delivered, oldest)
	}
}

func allAcked(pd *Pending, expected []string) bool {
	for _, d := range expected {
		if !pd.Acked[d] {
			return false
		}
	}
	return true
}

// mustHoldLocked decides whether the relay needs the data: yes if some device
// that lacks it is offline (nobody else can deliver it later), or if connected
// devices are taking too long to sync among themselves.
func (r *Relay) mustHoldLocked(pd *Pending, expected []string, now time.Time) bool {
	for _, d := range expected {
		if !pd.Acked[d] && r.peers[d] == nil {
			return true
		}
	}
	return now.Sub(pd.Since) > HoldTime
}

// fetch downloads the blocks of pd from a device that holds the file.
func (r *Relay) fetch(name string, pd *Pending) {
	defer func() {
		r.mu.Lock()
		delete(r.fetching, name)
		r.mu.Unlock()
	}()

	r.mu.Lock()
	var src *node.Peer
	for id := range pd.Acked {
		if p := r.peers[id]; p != nil && alive(p) {
			src = p
			break
		}
	}
	var missing []int
	var need int64
	for i, h := range pd.Entry.CBlocks {
		if !r.hasBlock(h) {
			missing = append(missing, i)
			_, sz := pd.Entry.BlockRange(i)
			need += int64(sz)
		}
	}
	over := src != nil && r.storeBytesLocked()+need > r.cfg.Quota()
	if over && !r.overflow {
		r.overflow = true
		r.log.Warn("relay storage quota reached; change not stored until space is freed", "change", short(name), "need_bytes", need)
	}
	r.mu.Unlock()
	if src == nil || over {
		return // retried at the next tick
	}

	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var failed error
	for w := 0; w < fetchWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				off, size := pd.Entry.BlockRange(i)
				rctx, c := context.WithTimeout(ctx, 2*time.Minute)
				data, err := src.Request(rctx, protocol.Request{Folder: r.cfg.ID, Name: name, Offset: off,
					Size: size, Hash: pd.Entry.CBlocks[i], Sealed: true})
				c()
				if err == nil {
					err = r.writeBlock(pd.Entry.CBlocks[i], data)
				}
				if err != nil {
					once.Do(func() { failed = err; cancel() })
					return
				}
			}
		}()
	}
feed:
	for _, i := range missing {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if failed != nil {
		r.log.Debug("fetch failed, will retry", "change", short(name), "err", failed)
		return
	}
	if r.ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	if r.pending[name] == pd { // not superseded meanwhile
		pd.Have = true
		r.overflow = false
		_ = r.saveLocked()
		r.mu.Unlock()
		r.log.Info("change stored on relay, waiting for absent devices", "change", short(name))
		r.announce(name)
		return
	}
	r.mu.Unlock()
}

// ---- probing ----------------------------------------------------------------

func (r *Relay) probeLoop() {
	t := time.NewTicker(ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.mu.Lock()
			peers := make([]*node.Peer, 0, len(r.peers))
			for _, p := range r.peers {
				peers = append(peers, p)
			}
			r.mu.Unlock()
			for _, p := range peers {
				go r.probePeer(p)
			}
		case <-r.ctx.Done():
			return
		}
	}
}

// probePeer asks a device which pending versions it already holds. This lets
// the relay notice deliveries that happened without it (direct sync between
// devices) and clean up after itself.
func (r *Relay) probePeer(p *node.Peer) {
	r.mu.Lock()
	var items []protocol.ProbeItem
	for name, pd := range r.pending {
		if !pd.Acked[p.ID()] {
			items = append(items, protocol.ProbeItem{Name: name, Version: pd.Entry.Version})
		}
	}
	r.mu.Unlock()
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	have, err := p.Probe(ctx, r.cfg.ID, items)
	if err != nil {
		return
	}
	r.mu.Lock()
	for i, ok := range have {
		if !ok {
			continue
		}
		if pd := r.pending[items[i].Name]; pd != nil && pd.Entry.Version.Compare(items[i].Version) == model.Equal {
			pd.Acked[p.ID()] = true
		}
	}
	_ = r.saveLocked()
	r.mu.Unlock()
	r.kick()
}

// ---- persistence and block store -------------------------------------------

// saveLocked writes pending.json (and the delivered table) durably: a unique
// temporary file, flushed to disk, then renamed. It returns the error so that a
// caller can refuse to acknowledge a change that was not actually stored.
func (r *Relay) saveLocked() error {
	b, err := json.Marshal(r.pending)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(r.dir, "pending.json"), b, 0o600); err != nil {
		r.log.Error("cannot save pending changes", "err", err)
		return err
	}
	if db, derr := json.Marshal(r.delivered); derr == nil {
		// Best effort: a lost table only costs one redundant download.
		_ = fsutil.WriteFileAtomic(filepath.Join(r.dir, "delivered.json"), db, 0o600)
	}
	return nil
}

// verifyBlocksLocked recomputes, after a restart, which changes really have all
// their data on disk. A crash can leave a missing or short block while the
// metadata says otherwise.
func (r *Relay) verifyBlocksLocked() {
	for id, pd := range r.pending {
		have := true
		for i, h := range pd.Entry.CBlocks {
			_, size := pd.Entry.BlockRange(i)
			fi, err := os.Stat(r.blockPath(h))
			if err != nil || fi.Size() != int64(size+protocol.SealOverhead) {
				have = false
				break
			}
		}
		if pd.Have && !have {
			r.log.Warn("stored data is incomplete after a restart; fetching it again", "change", short(id))
		}
		pd.Have = have
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// validSealed rejects malformed entries from a peer before they can influence
// storage decisions.
func validSealed(e protocol.SealedEntry) bool {
	if len(e.ID) != 32 || e.Version == nil || len(e.Meta) == 0 || len(e.Meta) > 1<<20 || e.Size < 0 {
		return false
	}
	if e.Deleted || len(e.CBlocks) == 0 {
		return len(e.CBlocks) == 0 && e.Size == 0
	}
	if e.BlockSize <= 0 || e.BlockSize > 16<<20 || int64(len(e.CBlocks)) != (e.Size+int64(e.BlockSize)-1)/int64(e.BlockSize) {
		return false
	}
	for _, h := range e.CBlocks {
		if len(h) != 32 {
			return false
		}
	}
	return true
}

// alive reports whether a session is still open. A table entry can briefly
// outlive its connection; never pick it as a source.
func alive(p *node.Peer) bool {
	select {
	case <-p.Done():
		return false
	default:
		return true
	}
}
