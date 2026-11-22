package service

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func lockUpdate(file *os.File) error {
	var region windows.Overlapped
	// Lock the first byte even on an empty file. Closing the owning handle
	// automatically releases the lock.
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
}

func prepareUpdateLock(path string) error { return createManagedDirectory(filepath.Dir(path)) }
