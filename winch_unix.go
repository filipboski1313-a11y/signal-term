//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func watchTerminalResize(stop <-chan struct{}) <-chan struct{} {
	out := make(chan struct{}, 4)
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-stop:
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}
