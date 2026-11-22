package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsControlRejectsWrongServer(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\btawg-control-test-%d`, os.Getpid())
	ln, err := listenControl(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if second, err := listenControl(path); err == nil {
		second.Close()
		t.Fatal("second local process could open a listener on the same control name")
	}
	seen := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var b [1]byte
			if n, readErr := c.Read(b[:]); n != 0 || readErr == nil {
				err = errors.New("request bytes reached unauthenticated pipe")
			} else if e, ok := readErr.(net.Error); ok && e.Timeout() {
				err = readErr
			}
		}
		seen <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := dialVerifiedPipe(ctx, path, uint32(os.Getpid())+1)
	if err == nil {
		c.Close()
		t.Fatal("accepted a pipe with an unrelated server PID")
	}
	if errors.Is(err, ErrNotRunning) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("mismatched pipe was treated as offline: %v", err)
	}
	select {
	case err := <-seen:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func testControlEndpoint(t *testing.T, _ string) string {
	return fmt.Sprintf(`\\.\pipe\btawg-test-%d-%s`, os.Getpid(), filepath.Base(t.Name()))
}
