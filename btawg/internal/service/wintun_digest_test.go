package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsBundleRejectsUnverifiedDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wintun.dll")
	if err := os.WriteFile(path, []byte("not the pinned Wintun driver"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsDriver(path); err == nil {
		t.Fatal("accepted a driver different from the signed release asset")
	}
}

func TestWindowsBundleAcceptsNativeDriver(t *testing.T) {
	path := os.Getenv("BTAWG_WINDOWS_BUNDLE_DRIVER")
	if path == "" {
		t.Skip("requires the signed wintun.dll from the release bundle")
	}
	if err := verifyWindowsDriver(path); err != nil {
		t.Fatal(err)
	}
}
