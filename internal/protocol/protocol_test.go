package protocol

import (
	"bytes"
	"net"
	"testing"
	"time"
)

type pipeStream struct{ net.Conn }

func TestRecvMaxRefusesOversizedFramesBeforeReadingThem(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		// Claims a 50 MB frame but sends nothing more: the reader must not wait for it.
		_, _ = a.Write([]byte{0x03, 0x0d, 0x40, 0x00})
	}()
	c := NewConn(pipeStream{b})
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	if _, _, _, err := c.RecvMax(4096); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("reader waited for the body of a frame it should have refused")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go NewConn(pipeStream{a}).Send(TypePair, PairRequest{Token: "t", Name: "n", Port: 7}, []byte("body"))
	typ, hdr, body, err := NewConn(pipeStream{b}).RecvMax(4096)
	if err != nil || typ != TypePair || !bytes.Contains(hdr, []byte(`"token":"t"`)) || string(body) != "body" {
		t.Fatalf("%v %v %s %v", typ, string(hdr), body, err)
	}
}
