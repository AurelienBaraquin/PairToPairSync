// Package folder implements the engine for a folder kept as a complete copy in
// a normal directory: it watches the directory, maintains the index, exchanges
// it with peers and downloads what is missing.
package folder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// Tunables, overridable in tests.
var (
	WatchDebounce = 300 * time.Millisecond
	PullRetryMax  = 30 * time.Second
)

const indexBatch = 1000

type want struct {
	entry    model.FileInfo
	sources  map[string]bool
	conflict bool // the local version lost a concurrent edit and must be preserved
	failures int
	retryAt  time.Time
}

type Folder struct {
	cfg   config.Folder
	self  string
	short string
	root  string
	log   *slog.Logger
	store *Store

	ignore atomic.Pointer[Matcher]

	mu     sync.Mutex
	peers  map[string]*node.Peer
	wanted map[string]*want
	status string // idle | scanning | syncing | error
	err    string

	scanMu  sync.Mutex
	scanned chan struct{}
	kick    chan struct{}
	dirty   chan string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates the engine. stateDir is where the index of this folder is kept.
func New(cfg config.Folder, selfID, stateDir string, log *slog.Logger) (*Folder, error) {
	store, err := openStore(filepath.Join(stateDir, "index-"+cfg.ID+".json"))
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	f := &Folder{
		cfg: cfg, self: selfID, short: identity.Short(selfID), root: filepath.Clean(cfg.Path),
		log: log.With("folder", cfg.ID), store: store,
		peers: map[string]*node.Peer{}, wanted: map[string]*want{}, status: "idle",
		scanned: make(chan struct{}), kick: make(chan struct{}, 1), dirty: make(chan string, 4096),
	}
	f.ignore.Store(&Matcher{})
	return f, nil
}

func (f *Folder) ID() string { return f.cfg.ID }

// Start prepares the directory and launches the background loops.
func (f *Folder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)
	if err := f.prepareRoot(); err != nil {
		f.setError(err)
		close(f.scanned)
		return nil // a broken folder must not stop the other ones
	}
	f.ignore.Store(loadIgnore(f.root))
	f.wg.Add(3)
	go func() { defer f.wg.Done(); f.scanLoop() }()
	go func() { defer f.wg.Done(); f.pullLoop() }()
	go func() { defer f.wg.Done(); f.saveLoop() }()
	return nil
}

func (f *Folder) Stop() {
	if f.cancel == nil {
		return
	}
	f.cancel()
	f.wg.Wait()
	if err := f.store.Save(); err != nil {
		f.log.Error("saving index failed", "err", err)
	}
}

// prepareRoot creates the folder and its marker on first use. If the index
// knows files but the marker vanished (unmounted drive, deleted folder), it
// refuses to continue instead of announcing a mass deletion.
func (f *Folder) prepareRoot() error {
	marker := filepath.Join(f.root, MetaDir)
	_, err := os.Stat(marker)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if len(f.store.All()) > 0 {
			return fmt.Errorf("folder marker %s is missing although the folder was synchronised before; "+
				"is the drive mounted? recreate the %s directory to confirm", marker, MetaDir)
		}
		if err := os.MkdirAll(marker, 0o755); err != nil {
			return err
		}
	default:
		return err
	}
	return os.MkdirAll(filepath.Join(marker, "tmp"), 0o755)
}

func (f *Folder) setError(err error) {
	f.mu.Lock()
	f.status, f.err = "error", err.Error()
	f.mu.Unlock()
	f.log.Error("folder stopped", "err", err)
}

func (f *Folder) setStatus(s string) {
	f.mu.Lock()
	if f.status != "error" {
		f.status = s
	}
	f.mu.Unlock()
}

func (f *Folder) saveLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := f.store.Save(); err != nil {
				f.log.Error("saving index failed", "err", err)
			}
		case <-f.ctx.Done():
			return
		}
	}
}

func (f *Folder) waitScanned() bool {
	select {
	case <-f.scanned:
		return f.ctx.Err() == nil
	case <-f.ctx.Done():
		return false
	}
}

// ---- node.FolderHandler -----------------------------------------------------

func (f *Folder) PeerUp(p *node.Peer) {
	if !f.waitScanned() || f.isBroken() {
		return
	}
	f.mu.Lock()
	f.peers[p.ID()] = p
	f.mu.Unlock()

	var entries []model.FileInfo
	if f.cfg.IsRelay(p.ID()) {
		// A relay only needs what changed since it last confirmed receipt. When
		// it is new, existing content is not uploaded: only future changes travel
		// through it.
		sent, ok := f.store.RelaySent(p.ID())
		if !ok {
			sent = f.store.Seq()
			f.store.SetRelaySent(p.ID(), sent)
		}
		entries = f.store.Since(sent)
	} else {
		entries = f.store.All()
	}
	f.sendTo(p, entries)
	f.kickPull()
}

func (f *Folder) PeerDown(p *node.Peer) {
	f.mu.Lock()
	if f.peers[p.ID()] == p {
		delete(f.peers, p.ID())
	}
	f.mu.Unlock()
}

func (f *Folder) isBroken() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status == "error"
}

func (f *Folder) sendTo(p *node.Peer, entries []model.FileInfo) {
	for len(entries) > 0 {
		n := min(indexBatch, len(entries))
		batch := entries[:n]
		entries = entries[n:]
		var maxSeq int64
		for _, e := range batch {
			maxSeq = max(maxSeq, e.Seq)
		}
		if err := p.SendIndex(f.cfg.ID, batch, maxSeq); err != nil {
			return
		}
	}
}

// broadcast announces local changes to every connected peer, relays included
// (for a relay, receiving an entry is also how it learns we hold that version).
func (f *Folder) broadcast(entries []model.FileInfo) {
	if len(entries) == 0 {
		return
	}
	f.mu.Lock()
	peers := make([]*node.Peer, 0, len(f.peers))
	for _, p := range f.peers {
		peers = append(peers, p)
	}
	f.mu.Unlock()
	for _, p := range peers {
		go f.sendTo(p, entries)
	}
}

func (f *Folder) HandleIndexAck(p *node.Peer, ack protocol.IndexAck) {
	if f.cfg.IsRelay(p.ID()) && ack.MaxSeq > 0 {
		f.store.SetRelaySent(p.ID(), ack.MaxSeq)
	}
}

func (f *Folder) HandleProbe(_ *node.Peer, items []protocol.ProbeItem) []bool {
	have := make([]bool, len(items))
	for i, it := range items {
		if cur, ok := f.store.Get(it.Name); ok && cur.Version.Dominates(it.Version) {
			have[i] = true
		}
	}
	return have
}

// HandleRequest serves one block of a file we hold.
func (f *Folder) HandleRequest(_ *node.Peer, r protocol.Request) ([]byte, string) {
	if f.isBroken() || r.Size <= 0 || r.Size > 32<<20 || r.Offset < 0 {
		return nil, protocol.ErrNotFound
	}
	cur, ok := f.store.Get(r.Name)
	if !ok || cur.Deleted || cur.Type != model.TypeFile {
		return nil, protocol.ErrNotFound
	}
	p, err := f.abs(r.Name)
	if err != nil {
		return nil, protocol.ErrNotFound
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, protocol.ErrNotFound
	}
	defer file.Close()
	buf := make([]byte, r.Size)
	if n, err := file.ReadAt(buf, r.Offset); n != r.Size {
		_ = err
		return nil, protocol.ErrChanged
	}
	if string(model.HashBytes(buf)) != string(r.Hash) {
		return nil, protocol.ErrChanged // file was edited since it was indexed
	}
	return buf, protocol.ErrNone
}

// Status is what the local API reports for this folder.
type Status struct {
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	Path       string   `json:"path"`
	Role       string   `json:"role"`
	State      string   `json:"state"`
	Error      string   `json:"error,omitempty"`
	LocalFiles int      `json:"local_files"`
	LocalBytes int64    `json:"local_bytes"`
	NeedFiles  int      `json:"need_files"`
	NeedBytes  int64    `json:"need_bytes"`
	Peers      []string `json:"connected_peers"`
}

func (f *Folder) Status() any {
	st := Status{ID: f.cfg.ID, Label: f.cfg.Label, Path: f.cfg.Path, Role: config.RoleFull}
	for _, e := range f.store.All() {
		if !e.Deleted && e.Type == model.TypeFile {
			st.LocalFiles++
			st.LocalBytes += e.Size
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st.State, st.Error = f.status, f.err
	for _, w := range f.wanted {
		st.NeedFiles++
		if !w.entry.Deleted {
			st.NeedBytes += w.entry.Size
		}
	}
	for _, p := range f.peers {
		st.Peers = append(st.Peers, p.Name())
	}
	return st
}
