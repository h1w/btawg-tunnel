package client

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// ManagedConfigPath is the root-owned client configuration file.
	ManagedConfigPath = "/etc/btawg/client.json"
	// ControlSocketPath is the root-only managed daemon socket.
	ControlSocketPath = "/run/btawg-client/control.sock"
)

func listenControl(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func dialControl(path string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, ErrNotRunning
	}
	return conn, err
}

func lockOfflineConfig() (func(), error) { return func() {}, nil }
