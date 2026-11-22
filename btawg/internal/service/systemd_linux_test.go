package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitRender(t *testing.T) {
	got := Server.renderUnit(nil)
	if !strings.Contains(got, "ExecStart=/usr/local/bin/btawg-server run -managed") {
		t.Fatalf("server unit missing managed run: %s", got)
	}
	if !strings.Contains(got, "StateDirectory=btawg") {
		t.Fatalf("server unit missing StateDirectory: %s", got)
	}
	if !strings.Contains(got, "Type=notify") {
		t.Fatalf("server unit missing Type=notify: %s", got)
	}

	got = Client.renderUnit(nil)
	if !strings.Contains(got, "ExecStart=/usr/local/bin/btawg-client run -managed") {
		t.Fatalf("client unit missing managed run: %s", got)
	}
	if !strings.Contains(got, "RuntimeDirectory=btawg-client") {
		t.Fatalf("client unit missing RuntimeDirectory: %s", got)
	}

	got = Server.renderUnit([]string{"-foo", "bar"})
	if !strings.Contains(got, `ExecStart=/usr/local/bin/btawg-server run -managed "-foo" "bar"`) {
		t.Fatalf("server unit missing extra args: %s", got)
	}
}

func TestInstallCopiesBinaryAndWritesUnit(t *testing.T) {
	oldUnitDir, oldBinDir := unitDir, binDir
	unitDir = t.TempDir()
	binDir = t.TempDir()
	t.Cleanup(func() { unitDir, binDir = oldUnitDir, oldBinDir })

	calls := []string{}
	oldRunner := systemctlRunner
	systemctlRunner = func(ctx context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	}
	t.Cleanup(func() { systemctlRunner = oldRunner })

	dummy := filepath.Join(t.TempDir(), "btawg-server")
	if err := os.WriteFile(dummy, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write dummy: %v", err)
	}
	// os.Executable in tests points to the test binary; temporarily replace it
	// by copying the dummy over the expected source path is awkward. Instead
	// use the real test binary as source and verify destination contents.
	if err := Server.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	dest := filepath.Join(binDir, "btawg-server")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("binary not copied: %v", err)
	}
	unitPath := filepath.Join(unitDir, "btawg-server.service")
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit not written: %v", err)
	}

	want := []string{"daemon-reload", "enable btawg-server.service", "restart btawg-server.service"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestControlActions(t *testing.T) {
	oldRunner := systemctlRunner
	systemctlRunner = func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "start" && args[1] == "btawg-server.service" {
			return "", nil
		}
		return "", nil
	}
	t.Cleanup(func() { systemctlRunner = oldRunner })

	if err := Server.Control("start"); err != nil {
		t.Fatalf("Control start: %v", err)
	}
	if err := Server.Control("bad"); err == nil {
		t.Fatal("Control bad should error")
	}
}

func TestUnitPassesSystemdAnalyze(t *testing.T) {
	oldBinDir := binDir
	binDir = t.TempDir()
	t.Cleanup(func() { binDir = oldBinDir })
	dummy := filepath.Join(binDir, "btawg-server")
	if err := os.WriteFile(dummy, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatalf("write dummy binary: %v", err)
	}

	unit := Server.renderUnit(nil)
	path := filepath.Join(t.TempDir(), "btawg-server.service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	out, err := exec.Command("systemd-analyze", "verify", path).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, string(out))
	}
}

func TestStateParsesEnabled(t *testing.T) {
	oldRunner := systemctlRunner
	systemctlRunner = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "show" {
			return "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { systemctlRunner = oldRunner })

	state, enabled, err := Server.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != "active" {
		t.Fatalf("state = %q, want active", state)
	}
	if !enabled {
		t.Fatal("want enabled")
	}
}

func TestStateDistinguishesMissingUnitFromStopped(t *testing.T) {
	oldRunner := systemctlRunner
	t.Cleanup(func() { systemctlRunner = oldRunner })
	for _, tc := range []struct {
		load, file, want string
	}{
		{"not-found", "", "not installed"},
		{"loaded", "disabled", "inactive"},
	} {
		t.Run(tc.load, func(t *testing.T) {
			systemctlRunner = func(_ context.Context, args ...string) (string, error) {
				return "LoadState=" + tc.load + "\nActiveState=inactive\nUnitFileState=" + tc.file + "\n", nil
			}
			state, enabled, err := Server.State()
			if err != nil || state != tc.want || enabled {
				t.Fatalf("state = %q, enabled = %v, err = %v; want %q, false, nil", state, enabled, err, tc.want)
			}
		})
	}
}
