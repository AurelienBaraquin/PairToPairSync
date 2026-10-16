// Package protocol defines the wire format spoken between devices over TLS.
//
// Every message is one frame:
//
//	uint32 length | uint8 type | uint32 headerLen | header (JSON) | body (raw bytes)
//
// where length counts everything after itself. Only Response frames carry a body.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/grant"
	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

const Version = 1

type Type uint8

const (
	TypeHello Type = iota + 1
	TypeIndex
	TypeIndexAck
	TypeRequest
	TypeResponse
	TypeProbe
	TypeProbeReply
	TypePing
	TypeGrants
)

const MaxFrame = 64 << 20

// SealOverhead is the size added to a block by authenticated encryption (Poly1305 tag).
const SealOverhead = 16

// Hello is the first message each side sends.
type Hello struct {
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Folders []string `json:"folders"` // IDs of the folders the sender shares with the receiver
}

// Index carries file metadata. Between full devices it carries plain Entries.
// Everything exchanged with a relay is Sealed instead: the relay is not trusted
// with names or content. MaxSeq is the highest local sequence number of the
// entries it contains (0 if not applicable); the receiver echoes it in an
// IndexAck once the entries are durably processed.
type Index struct {
	Folder  string           `json:"folder"`
	Entries []model.FileInfo `json:"entries,omitempty"`
	Sealed  []SealedEntry    `json:"sealed,omitempty"`
	MaxSeq  int64            `json:"max_seq,omitempty"`
}

// SealedEntry is the only view of a change a relay ever has. In clear: an opaque
// path identifier, the version vector (so the relay can order changes), the
// timestamp, the deletion flag and the shape of the data. Names, modes and
// plaintext block hashes are inside Meta, encrypted with the folder secret.
type SealedEntry struct {
	ID        string       `json:"id"`
	Version   model.Vector `json:"version"`
	ModTime   int64        `json:"mtime"`
	Deleted   bool         `json:"deleted,omitempty"`
	Size      int64        `json:"size"`
	BlockSize int          `json:"block_size,omitempty"`
	CBlocks   [][]byte     `json:"cblocks,omitempty"` // keyed identifier of each encrypted block
	Meta      []byte       `json:"meta"`
}

// WinsOver mirrors model.FileInfo.WinsOver using only what a relay can see, so
// that relay and devices agree on the canonical version of a concurrent edit.
func (e SealedEntry) WinsOver(o SealedEntry) bool {
	if e.Deleted != o.Deleted {
		return !e.Deleted
	}
	if e.ModTime != o.ModTime {
		return e.ModTime > o.ModTime
	}
	return e.Version.String() > o.Version.String()
}

// BlockRange returns the plaintext byte range covered by block i.
func (e SealedEntry) BlockRange(i int) (off int64, size int) {
	off = int64(i) * int64(e.BlockSize)
	size = e.BlockSize
	if rest := e.Size - off; rest < int64(size) {
		size = int(rest)
	}
	return off, size
}

type IndexAck struct {
	Folder string `json:"folder"`
	MaxSeq int64  `json:"max_seq"`
}

// Request asks for one block of a file. Hash identifies the block by content,
// which is what a relay (that has no directory tree) uses to find it.
type Request struct {
	Folder string `json:"folder"`
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
	Hash   []byte `json:"hash"`
	// Sealed requests address a block of an encrypted relay entry: Name is the
	// opaque path ID, Hash the keyed block ID, and the reply is ciphertext.
	Sealed bool `json:"sealed,omitempty"`
}

// Response error codes.
const (
	ErrNone     = ""
	ErrNotFound = "not_found"
	ErrChanged  = "changed"
	ErrInternal = "internal"
)

type Response struct {
	Err string `json:"err,omitempty"`
}

// ProbeItem asks "do you hold at least this version of this path?".
type ProbeItem struct {
	Name    string       `json:"name"`
	Version model.Vector `json:"version"`
}

type Probe struct {
	Folder string      `json:"folder"`
	Items  []ProbeItem `json:"items"`
}

type ProbeReply struct {
	Have []bool `json:"have"`
}

// Stream is the subset of a network connection or libp2p stream that Conn needs.
type Stream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Conn frames messages on top of a Stream.
type Conn struct {
	nc  Stream
	r   *bufio.Reader
	wmu sync.Mutex
}

func NewConn(nc Stream) *Conn { return &Conn{nc: nc, r: bufio.NewReaderSize(nc, 64<<10)} }

func (c *Conn) Close() error { return c.nc.Close() }

func (c *Conn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }

// Send writes one frame. hdr is JSON encoded; body may be nil.
func (c *Conn) Send(t Type, hdr any, body []byte) error {
	h, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	total := 1 + 4 + len(h) + len(body)
	if total > MaxFrame {
		return fmt.Errorf("frame too large (%d bytes)", total)
	}
	prefix := make([]byte, 9)
	binary.BigEndian.PutUint32(prefix[0:4], uint32(total))
	prefix[4] = byte(t)
	binary.BigEndian.PutUint32(prefix[5:9], uint32(len(h)))

	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	bufs := net.Buffers{prefix, h, body}
	_, err = bufs.WriteTo(c.nc)
	return err
}

// Recv reads one frame.
func (c *Conn) Recv() (t Type, hdr, body []byte, err error) { return c.RecvMax(MaxFrame) }

// RecvMax reads one frame of at most max bytes. The size is checked before
// anything is allocated, and the buffer grows only as bytes really arrive: a peer
// that merely claims a huge frame (and then trickles data) cannot make us reserve
// that memory.
func (c *Conn) RecvMax(max int) (t Type, hdr, body []byte, err error) {
	var p [4]byte
	if _, err = io.ReadFull(c.r, p[:]); err != nil {
		return
	}
	total := binary.BigEndian.Uint32(p[:])
	if total < 5 || int64(total) > int64(max) || total > MaxFrame {
		return 0, nil, nil, fmt.Errorf("bad frame length %d", total)
	}
	var b bytes.Buffer
	b.Grow(int(min(total, 64<<10)))
	if _, err = io.CopyN(&b, c.r, int64(total)); err != nil {
		return 0, nil, nil, err
	}
	buf := b.Bytes()
	t = Type(buf[0])
	hl := binary.BigEndian.Uint32(buf[1:5])
	if int64(hl) > int64(len(buf)-5) {
		return 0, nil, nil, errors.New("bad header length")
	}
	return t, buf[5 : 5+hl], buf[5+hl:], nil
}

// PairRequest is sent by a device that scanned an invitation. It is the only
// thing a not-yet-trusted device is allowed to say.
type PairRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Port  int    `json:"port,omitempty"` // port this device listens on, so the inviter can dial back
}

// PairResponse is the inviter's answer. Secret is only ever sent to a full
// device, over the authenticated, encrypted connection, in reply to a valid token.
type PairResponse struct {
	OK           bool   `json:"ok"`
	Err          string `json:"err,omitempty"`
	Name         string `json:"name,omitempty"`
	Folder       string `json:"folder,omitempty"`
	Label        string `json:"label,omitempty"`
	Secret       string `json:"secret,omitempty"`
	InviterRelay bool   `json:"inviter_relay,omitempty"`

	// Joining a network through a full device that introduces the guest (see package grant): the signed grants the
	// guest needs (its own, and those of the devices it shares folders with), the
	// secrets of the full folders the inviter holds, and where to find the devices.
	Network bool                `json:"network,omitempty"`
	Grants  []grant.Signed      `json:"grants,omitempty"`
	Secrets map[string]string   `json:"secrets,omitempty"`
	Hints   map[string][]string `json:"hints,omitempty"`
}

// Grants is exchanged between connected devices: the signed grants one of them
// holds that are of interest to the other, with hints on where to reach the
// devices they describe (addresses are untrusted: identities are verified when
// connecting).
type Grants struct {
	List  []grant.Signed      `json:"list"`
	Hints map[string][]string `json:"hints,omitempty"`
}

// Limits on what a connected device may send in a Grants message.
const (
	MaxGrants    = 600
	MaxHintedIDs = 128
	MaxHintAddrs = 4
)

// SecretRequest asks a full device of a folder for the folder secret. It is only
// answered for a device that the folder's members were told, by an introduction or by
// the owner, to treat as a full member.
type SecretRequest struct {
	Folder string `json:"folder"`
}

// SecretResponse answers a SecretRequest.
type SecretResponse struct {
	OK     bool   `json:"ok"`
	Secret string `json:"secret,omitempty"`
}

const TypeSecret Type = 21

// SecretProtocol is the libp2p protocol ID of the secret stream.
const SecretProtocol = "/p2psync/1.0/secret"

const TypePair Type = 20

// PairProtocol is the libp2p protocol ID of the pairing stream.
const PairProtocol = "/p2psync/1.0/pair"

// Approximate encoded sizes, used to keep an index message well under MaxFrame.
const (
	entryBase = 256 // JSON overhead of one entry
	blockWire = 48  // one base64 SHA-256 plus quotes and comma
)

// WireSize estimates the JSON size of a sealed entry.
func (e SealedEntry) WireSize() int {
	return entryBase + len(e.Meta)*4/3 + len(e.CBlocks)*blockWire + len(e.Version)*24
}
