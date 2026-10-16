//go:build !windows

package main

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, buf *syncBuf, want string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("log never said %q:\n%s", want, buf.String())
}

// A damaged config.json must not take a running daemon down, and fixing it must
// bring the new configuration in.
func TestRunSurvivesBrokenConfig(t *testing.T) {
	home := t.TempDir()
	if _, err := app.Init(home, "test", freePort(t), "-"); err != nil {
		t.Fatal(err)
	}
	buf := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	done := make(chan error, 1)
	go func() { done <- run(home, logger) }()

	waitFor(t, buf, "running")
	good, err := os.ReadFile(filepath.Join(home, app.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, app.ConfigFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, buf, "keeping the previous configuration")
	select {
	case err := <-done:
		t.Fatalf("run returned after a bad config: %v", err)
	default:
	}

	time.Sleep(1100 * time.Millisecond) // make sure the restored file has a newer mtime
	if err := os.WriteFile(filepath.Join(home, app.ConfigFile), good, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, buf, "configuration changed, restarting")

	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop")
	}
}

// Without any working configuration the daemon keeps waiting for a fix.
func TestRunWaitsForFirstGoodConfig(t *testing.T) {
	home := t.TempDir()
	if _, err := app.Init(home, "test", freePort(t), "-"); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(filepath.Join(home, app.ConfigFile))
	if err := os.WriteFile(filepath.Join(home, app.ConfigFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- run(home, slog.New(slog.NewTextHandler(buf, nil))) }()
	waitFor(t, buf, "waiting for a fix")

	time.Sleep(1100 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(home, app.ConfigFile), good, 0o600)
	waitFor(t, buf, "running")

	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop")
	}
}
