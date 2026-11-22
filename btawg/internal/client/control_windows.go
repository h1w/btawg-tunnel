package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"bittorrent-tunnel/btawg/internal/service"
)

const ControlSocketPath = `\\.\pipe\btawg-client-control`

var ManagedConfigPath = filepath.Join(service.ManagedDataDirectory(), "client.json")

func listenControl(path string) (net.Listener, error) {
	const acl = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
	if path != ControlSocketPath {
		return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: acl})
	}
	id, err := windows.GenerateGUID()
	if err != nil {
		return nil, err
	}
	name := path + "-" + id.String()
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: acl})
	if err != nil {
		return nil, err
	}
	if err := service.WriteManagedFile(controlRendezvous(), []byte(name)); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func controlRendezvous() string { return filepath.Join(service.ManagedDataDirectory(), "control.pipe") }

func dialControl(path string) (net.Conn, error) {
	pid := uint32(os.Getpid())
	managed := path == ControlSocketPath
	if managed {
		var err error
		pid, err = service.Client.ProcessID()
		if err != nil {
			return nil, err
		}
		if pid == 0 {
			return nil, ErrNotRunning
		}
		b, err := os.ReadFile(controlRendezvous())
		if err != nil {
			return nil, fmt.Errorf("locate running service control pipe: %w", err)
		}
		name := string(b)
		id, ok := strings.CutPrefix(name, ControlSocketPath+"-")
		if _, err := windows.GUIDFromString(id); !ok || err != nil {
			return nil, errors.New("invalid service control rendezvous")
		}
		path = name
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialVerifiedPipe(ctx, path, pid)
	if !managed && errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, ErrNotRunning
	}
	return conn, err
}

func dialVerifiedPipe(ctx context.Context, path string, expect uint32) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		if expect == 0 && (errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND)) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(conn.(interface{ Fd() uintptr }).Fd()), &pid); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("inspect control pipe server: %w", err)
	}
	if expect == 0 || pid != expect {
		_ = conn.Close()
		return nil, errors.New("control pipe server is not the managed client")
	}
	return conn, nil
}

func lockOfflineConfig() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return service.LockManagedConfig(ctx)
}
