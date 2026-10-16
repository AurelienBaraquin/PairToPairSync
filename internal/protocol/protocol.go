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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

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
)

const MaxFrame = 64 << 20

// Hello is the first message each side sends.
type Hello struct {
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Folders []string `json:"folders"` // IDs of the folders the sender shares with the receiver
}

// Index carries file metadata. MaxSeq is the highest local sequence number of
// the entries it contains (0 if not applicable); the receiver echoes it in an
// IndexAck once the entries are durably processed.
type Index struct {
	Folder  string           `json:"folder"`
	Entries []model.FileInfo `json:"entries"`
	MaxSeq  int64            `json:"max_seq,omitempty"`
}

type IndexAck struct {
	Folder string `json:"folder"`
	MaxSeq int64  `json:"max_seq"`
}

// Request asks for one block of a file. Hash identifies the block by content,
// which is what a relay (that has no directory tree) uses to find it.
type Request struct {
	ID     uint32 `json:"id"`
	Folder string `json:"folder"`
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
	Hash   []byte `json:"hash"`
}

// Response error codes.
const (
	ErrNone     = ""
	ErrNotFound = "not_found"
	ErrChanged  = "changed"
	ErrInternal = "internal"
)

type Response struct {
	ID  uint32 `json:"id"`
	Err string `json:"err,omitempty"`
}

// ProbeItem asks "do you hold at least this version of this path?".
type ProbeItem struct {
	Name    string       `json:"name"`
	Version model.Vector `json:"version"`
}

type Probe struct {
	ID     uint32      `json:"id"`
	Folder string      `json:"folder"`
	Items  []ProbeItem `json:"items"`
}

type ProbeReply struct {
	ID   uint32 `json:"id"`
	Have []bool `json:"have"`
}

// Conn frames messages on top of a net.Conn.
type Conn struct {
	nc  net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func NewConn(nc net.Conn) *Conn { return &Conn{nc: nc, r: bufio.NewReaderSize(nc, 64<<10)} }

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
func (c *Conn) Recv() (t Type, hdr, body []byte, err error) {
	var p [4]byte
	if _, err = io.ReadFull(c.r, p[:]); err != nil {
		return
	}
	total := binary.BigEndian.Uint32(p[:])
	if total < 5 || total > MaxFrame {
		return 0, nil, nil, fmt.Errorf("bad frame length %d", total)
	}
	buf := make([]byte, total)
	if _, err = io.ReadFull(c.r, buf); err != nil {
		return
	}
	t = Type(buf[0])
	hl := binary.BigEndian.Uint32(buf[1:5])
	if int(hl) > len(buf)-5 {
		return 0, nil, nil, errors.New("bad header length")
	}
	return t, buf[5 : 5+hl], buf[5+hl:], nil
}
