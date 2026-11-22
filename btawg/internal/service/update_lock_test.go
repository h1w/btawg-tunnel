package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateLockExcludesCompetingFileHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.update.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := lockUpdate(first); err != nil {
		t.Fatal(err)
	}
	if err := lockUpdate(second); err == nil {
		t.Fatal("competing updater acquired the same lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lockUpdate(second); err != nil {
		t.Fatalf("closing the owning handle did not release the lock: %v", err)
	}
}
