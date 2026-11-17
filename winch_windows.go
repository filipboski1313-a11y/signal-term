//go:build windows

package main

import (
	"os"
	"time"

	"golang.org/x/term"
)

func watchTerminalResize(stop <-chan struct{}) <-chan struct{} {
	out := make(chan struct{}, 4)
	go func() {
		fd := int(os.Stdout.Fd())
		lastW, lastH, _ := term.GetSize(fd)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				w, h, err := term.GetSize(fd)
				if err == nil && (w != lastW || h != lastH) {
					lastW, lastH = w, h
					select {
					case out <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return out
}
