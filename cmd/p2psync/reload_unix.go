//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyReload(ch chan struct{}) {
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGHUP)
	go func() {
		for range s {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
}
