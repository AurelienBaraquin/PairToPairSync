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
	"github.com/aurelienbaraquin/pairtopairsync/internal/seal"
)

// Tunables, overridable in tests.
var (
	WatchDebounce = 300 * time.Millisecond
	PullRetryMax  = 30 * time.Second
	serveDelay    atomic.Int64 // nanoseconds
)

// SetServeDelay slows block serving; it exists to simulate a slow link in tests.
func SetServeDelay(d time.Duration) { serveDelay.Store(int64(d)) }

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

	sent, recv atomic.Int64 // bytes of file data served / downloaded since start

	sealer *seal.Sealer // nil when the folder has no relay
	idMu   sync.Mutex
	idMap  map[string]string
	idSeq  int64

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
	if !config.ValidFolderID(cfg.ID) { // it becomes part of a file name
		return nil, fmt.Errorf("invalid folder id %q", cfg.ID)
	}
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
	if cfg.Secret != "" {
		if f.sealer, err = seal.New(cfg.Secret); err != nil {
			return nil, fmt.Errorf("folder secret: %w", err)
		}
	}
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
	tmp := filepath.Join(marker, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	// Forget abandoned partial downloads after a few days.
	if entries, err := os.ReadDir(tmp); err == nil {
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 3*24*time.Hour {
				_ = os.Remove(filepath.Join(tmp, e.Name()))
			}
		}
	}
	return nil
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
	select {
	case <-p.Done(): // the session ended (or was replaced) before PeerUp ran: do not register it
		f.mu.Unlock()
		return
	default:
	}
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
	toRelay := f.isRelay(p)
	for len(entries) > 0 {
		n := min(indexBatch, len(entries))
		batch := entries[:n]
		entries = entries[n:]
		var maxSeq int64
		for _, e := range batch {
			maxSeq = max(maxSeq, e.Seq)
		}
		idx := protocol.Index{Folder: f.cfg.ID, MaxSeq: maxSeq}
		if toRelay {
			if f.sealer == nil {
				return
			}
			for _, e := range batch {
				se, err := f.sealEntry(e)
				if err != nil {
					f.log.Error("cannot seal entry for relay", "err", err)
					return
				}
				idx.Sealed = append(idx.Sealed, se)
			}
		} else {
			idx.Entries = batch
		}
		if err := p.SendIndex(idx); err != nil {
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

func (f *Folder) HandleProbe(p *node.Peer, items []protocol.ProbeItem) []bool {
	have := make([]bool, len(items))
	if !f.isRelay(p) || f.sealer == nil {
		return have // only relays probe, and they speak in opaque IDs
	}
	for i, it := range items {
		name, ok := f.resolveID(it.Name)
		if !ok {
			continue
		}
		if cur, ok := f.store.Get(name); ok && cur.Version.Dominates(it.Version) {
			have[i] = true
		}
	}
	return have
}

// HandleRequest serves one block of a file we hold. Relays only ever get
// encrypted blocks; full devices get plain ones.
func (f *Folder) HandleRequest(p *node.Peer, r protocol.Request) ([]byte, string) {
	if f.isBroken() || r.Size <= 0 || r.Size > 32<<20 || r.Offset < 0 {
		return nil, protocol.ErrNotFound
	}
	if d := time.Duration(serveDelay.Load()); d > 0 {
		time.Sleep(d)
	}
	data, code := f.serveBlock(p, r)
	f.sent.Add(int64(len(data)))
	return data, code
}

func (f *Folder) serveBlock(p *node.Peer, r protocol.Request) ([]byte, string) {
	if f.isRelay(p) != r.Sealed {
		return nil, protocol.ErrNotFound // a relay must not receive plaintext, nor a full device ciphertext
	}
	if r.Sealed {
		if f.sealer == nil {
			return nil, protocol.ErrNotFound
		}
		return f.serveSealedBlock(r)
	}
	cur, ok := f.store.Get(r.Name)
	if !ok || cur.Deleted || cur.Type != model.TypeFile {
		return nil, protocol.ErrNotFound
	}
	buf, code := f.readBlock(r.Name, r.Offset, r.Size)
	if code != protocol.ErrNone {
		return nil, code
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
	SentBytes  int64    `json:"sent_bytes"`
	RecvBytes  int64    `json:"received_bytes"`
	Peers      []string `json:"connected_peers"`
}

func (f *Folder) Status() any {
	st := Status{ID: f.cfg.ID, Label: f.cfg.Label, Path: f.cfg.Path, Role: config.RoleFull,
		SentBytes: f.sent.Load(), RecvBytes: f.recv.Load()}
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
