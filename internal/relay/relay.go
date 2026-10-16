// Package relay implements the temporary store-and-forward role of a folder.
//
// A relay (typically a phone) has no copy of the synchronised tree. It only
// holds the latest, not yet delivered, changes: file metadata plus the data
// blocks, addressed by hash. As soon as every full device of the folder is known
// to hold a change, the relay deletes it. Nothing is kept permanently.
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

// Pending is one change waiting to be delivered to every full device.
type Pending struct {
	Entry model.FileInfo  `json:"entry"`
	Acked map[string]bool `json:"acked"` // devices known to hold this version (or a newer one)
	Have  bool            `json:"have"`  // all data blocks are stored here
	Since time.Time       `json:"since"`
}

type Relay struct {
	cfg  config.Folder
	self string
	dir  string
	log  *slog.Logger

	mu       sync.Mutex
	pending  map[string]*Pending
	peers    map[string]*node.Peer
	fetching map[string]bool
	overflow bool // quota warning already logged

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}
}

func New(cfg config.Folder, selfID string, log *slog.Logger) *Relay {
	return &Relay{
		cfg: cfg, self: selfID, dir: filepath.Clean(cfg.Path), log: log.With("folder", cfg.ID, "role", "relay"),
		pending: map[string]*Pending{}, peers: map[string]*node.Peer{}, fetching: map[string]bool{},
		wake: make(chan struct{}, 1),
	}
}

func (r *Relay) ID() string { return r.cfg.ID }

func (r *Relay) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(r.dir, "blocks"), 0o700); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(r.dir, "pending.json")); err == nil {
		if err := json.Unmarshal(b, &r.pending); err != nil {
			return fmt.Errorf("corrupt pending.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
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

func (r *Relay) announceableLocked(dev string) []model.FileInfo {
	var out []model.FileInfo
	for _, pd := range r.pending {
		if pd.Have && !pd.Acked[dev] {
			out = append(out, pd.Entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Relay) send(p *node.Peer, entries []model.FileInfo) {
	const batch = 500
	for len(entries) > 0 {
		n := min(batch, len(entries))
		if p.SendIndex(protocol.Index{Folder: r.cfg.ID, Entries: entries[:n]}) != nil {
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
		e []model.FileInfo
	}
	var jobs []job
	for id, p := range r.peers {
		var es []model.FileInfo
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

// HandleIndex receives changes announced by a full device.
func (r *Relay) HandleIndex(p *node.Peer, idx protocol.Index) {
	if r.cfg.IsRelay(p.ID()) {
		return
	}
	var announce []string
	r.mu.Lock()
	for _, e := range idx.Entries {
		if e.Version == nil || !validName(e.Name) {
			continue
		}
		e.Seq = 0
		cur := r.pending[e.Name]
		switch {
		case cur == nil:
			r.pending[e.Name] = r.newPendingLocked(e, p.ID())
			announce = append(announce, e.Name)
		default:
			switch cur.Entry.Version.Compare(e.Version) {
			case model.Equal, model.Greater:
				if cur.Entry.Version.Compare(e.Version) == model.Equal {
					cur.Acked[p.ID()] = true
				} else {
					announce = append(announce, e.Name) // peer is behind: tell it
				}
			case model.Lesser:
				r.pending[e.Name] = r.newPendingLocked(e, p.ID())
				announce = append(announce, e.Name)
			case model.Concurrent:
				// Keep only the canonical winner; the loser's device receives the
				// winner and preserves its own file as a conflict copy.
				if e.WinsOver(cur.Entry) {
					r.pending[e.Name] = r.newPendingLocked(e, p.ID())
				}
				announce = append(announce, e.Name)
			}
		}
	}
	r.gcBlocksLocked()
	r.saveLocked() // durable before the node acknowledges the index
	r.mu.Unlock()
	r.announce(announce...)
	r.kick()
}

func (r *Relay) newPendingLocked(e model.FileInfo, from string) *Pending {
	pd := &Pending{Entry: e, Acked: map[string]bool{from: true}, Since: time.Now()}
	pd.Have = true
	for _, h := range e.Blocks {
		if !r.hasBlock(h) {
			pd.Have = false
			break
		}
	}
	if e.Deleted || e.Type == model.TypeDir {
		pd.Have = true
	}
	return pd
}

func (r *Relay) HandleIndexAck(*node.Peer, protocol.IndexAck) {}

func (r *Relay) HandleProbe(_ *node.Peer, items []protocol.ProbeItem) []bool {
	return make([]bool, len(items))
}

// HandleRequest serves a stored block by its hash.
func (r *Relay) HandleRequest(_ *node.Peer, req protocol.Request) ([]byte, string) {
	if len(req.Hash) != 32 {
		return nil, protocol.ErrNotFound
	}
	b, err := os.ReadFile(r.blockPath(req.Hash))
	if err != nil || len(b) != req.Size || string(model.HashBytes(b)) != string(req.Hash) {
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
			r.log.Warn("dropping undelivered change after retention period", "file", name)
			delete(r.pending, name)
			removed = true
		case len(expected) > 0 && allAcked(pd, expected):
			r.log.Info("change delivered to every device, removed from relay", "file", name)
			delete(r.pending, name)
			removed = true
		}
	}
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
		r.gcBlocksLocked()
		r.saveLocked()
	}
	r.mu.Unlock()
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
		if p := r.peers[id]; p != nil {
			src = p
			break
		}
	}
	var missing []int
	var need int64
	for i, h := range pd.Entry.Blocks {
		if !r.hasBlock(h) {
			missing = append(missing, i)
			_, sz := pd.Entry.BlockRange(i)
			need += int64(sz)
		}
	}
	over := src != nil && r.storeBytesLocked()+need > r.cfg.Quota()
	if over && !r.overflow {
		r.overflow = true
		r.log.Warn("relay storage quota reached; change not stored until space is freed", "file", name, "need_bytes", need)
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
				data, err := src.Request(rctx, r.cfg.ID, name, off, size, pd.Entry.Blocks[i], false)
				c()
				if err == nil {
					err = r.writeBlock(pd.Entry.Blocks[i], data)
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
		r.log.Debug("fetch failed, will retry", "file", name, "err", failed)
		return
	}
	if r.ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	if r.pending[name] == pd { // not superseded meanwhile
		pd.Have = true
		r.overflow = false
		r.saveLocked()
		r.mu.Unlock()
		r.log.Info("change stored on relay, waiting for absent devices", "file", name)
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
	r.saveLocked()
	r.mu.Unlock()
	r.kick()
}

// ---- persistence and block store -------------------------------------------

func (r *Relay) saveLocked() {
	b, err := json.Marshal(r.pending)
	if err != nil {
		r.log.Error("cannot encode pending changes", "err", err)
		return
	}
	path := filepath.Join(r.dir, "pending.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		r.log.Error("cannot save pending changes", "err", err)
	}
}

func validName(name string) bool {
	return name != "" && name[0] != '/' && name != "." && len(name) < 4096
}
