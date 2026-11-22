package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
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
				return
			}
		}
	}
	t.Fatalf("recovery service did not restore the previous service: %q", WindowsUpdateNotice())
}
