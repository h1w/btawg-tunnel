package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"bittorrent-tunnel/btawg/internal/tunsetup"
)

func TestUpdateSCMActivatesVerifiedBundle(t *testing.T) {
	source := os.Getenv("BTAWG_WINDOWS_UPDATE_FIXTURE")
	if source == "" {
		t.Skip("requires a dedicated elevated Windows fixture with an installed client and next bundle")
	}
	state, automatic, err := Client.State()
	if err != nil || state != "active" {
		t.Fatalf("test fixture client must be running: %s, %v", state, err)
	}
	bundle, err := os.MkdirTemp(windowsInstallDirectory(), "bundle-")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range windowsBundleFiles {
		f, err := os.Open(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		err = writeFile(filepath.Join(bundle, name), f, 0o700)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyWindowsDriver(filepath.Join(bundle, "wintun.dll")); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bundle, "btawg-client.exe")
	old := Client.binaryPath()
	tunName := windowsManagedTUNName()
	formerAdapter, hadAdapter, err := tunsetup.FindWindowsAdapterIdentity(tunName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Client.handoffWindowsBundle(exe); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(50 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if s, auto, err := Client.State(); err == nil && s == "active" && auto == automatic && Client.binaryPath() == exe && strings.Contains(WindowsUpdateNotice(), "completed successfully") {
			if _, err := os.Stat(old); err != nil {
				t.Fatalf("previous verified image vanished before it could recover: %v", err)
			}
			m, err := mgr.Connect()
			if err != nil {
				t.Fatal(err)
			}
			defer m.Disconnect()
			recovery, err := m.OpenService(windowsRecoveryService)
			if err != nil {
				t.Fatal(err)
			}
			defer recovery.Close()
			cfg, err := recovery.Config()
			if err != nil || cfg.StartType != mgr.StartAutomatic {
				t.Fatalf("recovery autostart: %v, %v", cfg.StartType, err)
			}
			args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
			if err != nil || len(args) != 2 || args[1] != "update-recovery" || !strings.EqualFold(args[0], exe) {
				t.Fatalf("recovery image = %q, want verified bundle %q: %v", cfg.BinaryPathName, exe, err)
			}
			status, err := recovery.Query()
			if err != nil || status.State != svc.Running || status.ProcessId == 0 {
				t.Fatalf("recovery monitor not running: %v pid=%d", err, status.ProcessId)
			}
			image, err := windowsProcessImage(status.ProcessId)
			if err != nil || !strings.EqualFold(image, exe) {
				t.Fatalf("running recovery image = %q, want %q: %v", image, exe, err)
			}
			if hadAdapter {
				if err := tunsetup.WaitForWindowsAdapterRemoval(t.Context(), formerAdapter); err != nil {
					t.Fatalf("former adapter still present after update: %v", err)
				}
				current, ok, err := tunsetup.FindWindowsAdapterIdentity(tunName)
				if err != nil || !ok {
					t.Fatalf("replacement adapter missing: %v", err)
				}
				if current.GUID == formerAdapter.GUID {
					t.Fatalf("update reused former adapter %s", formerAdapter.GUID)
				}
			}
			return
		}
	}
	t.Fatalf("update did not commit: path=%q result=%q", Client.binaryPath(), WindowsUpdateNotice())
}

func TestUpdateSCMRestoresAbandonedBundleSelection(t *testing.T) {
	if os.Getenv("BTAWG_WINDOWS_UPDATE_FIXTURE") == "" {
		t.Skip("requires the elevated SCM update fixture")
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(Client.unitName())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	formerAdapter, hadAdapter, err := tunsetup.FindWindowsAdapterIdentity(windowsManagedTUNName())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := windowsLifecycleLock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	proposed := windows.ComposeCommandLine([]string{filepath.Join(windowsInstallDirectory(), "bundle-interrupted-fixture", "btawg-client.exe"), "run", "-managed"})
	interrupted := func() error {
		defer unlock()
		if err := saveWindowsTransition(windowsUpdateTransition{OldCommand: old.BinaryPathName, NewCommand: proposed, WasRunning: true, StartType: old.StartType}); err != nil {
			return err
		}
		if err := stopService(s); err != nil {
			return err
		}
		return setServiceCommand(s, proposed)
	}
	if err := interrupted(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		cfg, err := s.Config()
		if err == nil && cfg.BinaryPathName == old.BinaryPathName && strings.Contains(WindowsUpdateNotice(), "previous bundle was restored") {
			if state, _, err := Client.State(); err == nil && state == "active" {
				if hadAdapter {
					if err := tunsetup.WaitForWindowsAdapterRemoval(t.Context(), formerAdapter); err != nil {
						t.Fatalf("former adapter still present after rollback: %v", err)
					}
					current, ok, err := tunsetup.FindWindowsAdapterIdentity(windowsManagedTUNName())
					if err != nil || !ok {
						t.Fatalf("restored adapter missing: %v", err)
					}
					if current.GUID == formerAdapter.GUID {
						t.Fatalf("rollback reused former adapter %s", formerAdapter.GUID)
					}
				}
				return
			}
		}
	}
	t.Fatalf("recovery service did not restore the previous service: %q", WindowsUpdateNotice())
}
