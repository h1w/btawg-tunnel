package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const managedDirectoryACL = "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

func Privileged() bool { return windows.GetCurrentProcessToken().IsElevated() }

func ManagedDataDirectory() string {
	path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		// Keep a machine-local absolute path if shell folder lookup fails.
		return filepath.Join(windowsDirectoryVolume(), `\ProgramData\btawg`)
	}
	return filepath.Join(path, "btawg")
}

func windowsDirectoryVolume() string {
	path, err := windows.GetWindowsDirectory()
	if err != nil {
		return `C:`
	}
	return filepath.VolumeName(path)
}

func createManagedDirectory(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(managedDirectoryACL)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	attrs := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(name, attrs); err == nil {
		return nil
	} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("not an owned btawg directory: %s", dir)
	}
	got, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if got.String() != sd.String() {
		return fmt.Errorf("unexpected owner or permissions on btawg directory %s", dir)
	}
	return nil
}

// WriteManagedFile replaces a private client configuration atomically. The
// staging file inherits the same protected ACL before it receives any data.
func WriteManagedFile(path string, data []byte) error {
	if err := createManagedDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return WriteFile(path, data, 0o600)
}

// LockManagedConfig serializes offline mutations with SCM startup.
func LockManagedConfig(ctx context.Context) (func(), error) {
	dir := ManagedDataDirectory()
	if err := createManagedDirectory(dir); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "client.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := lockUpdate(file)
		if err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("lock client configuration: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
