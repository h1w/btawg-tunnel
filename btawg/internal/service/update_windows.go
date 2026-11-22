package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func (u Unit) updateLockPath() string {
	return filepath.Join(windowsInstallDirectory(), "download.lock")
}

func (u Unit) applyRelease(ctx context.Context, client *http.Client, tag string, progress func(string)) error {
	if u != Client {
		return fmt.Errorf("no Windows release for %s", u)
	}
	base := assetBase + tag + "/"
	progress("Fetching SHA-256 checksum…")
	want, err := releaseChecksum(ctx, client, base, windowsReleaseAsset)
	if err != nil {
		return fmt.Errorf("get release checksum: %w", err)
	}
	root := windowsInstallDirectory()
	if err := createManagedDirectory(root); err != nil {
		return err
	}
	bundle, err := os.MkdirTemp(root, "bundle-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(bundle)
		}
	}()
	archive := filepath.Join(bundle, windowsReleaseAsset)
	if err := downloadBinary(ctx, client, base+windowsReleaseAsset, archive, want, progress); err != nil {
		return err
	}
	if err := unpackWindowsBundle(archive, bundle); err != nil {
		return err
	}
	if err := verifyWindowsDriver(filepath.Join(bundle, "wintun.dll")); err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil {
		return err
	}
	exe := filepath.Join(bundle, "btawg-client.exe")
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := exec.CommandContext(checkCtx, exe, "--version").Output()
	cancel()
	if err != nil || strings.TrimSpace(string(out)) != tag {
		return fmt.Errorf("staged Windows client does not report release %s", tag)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	progress("Handing verified Windows bundle to the update helper…")
	// The recovery service may retain this image even when handoff fails.
	keep = true
	_, err = u.handoffWindowsBundle(exe)
	return err
}

// The selected command points to an immutable bundle; the old bundle remains
// available throughout activation and recovery.
func (u Unit) handoffWindowsBundle(exe string) (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(u.unitName())
	if err != nil {
		return false, err
	}
	defer s.Close()
	if err := u.verifyInstalledService(s); err != nil {
		return false, err
	}
	if err := ensureWindowsRecoveryService(m, s, exe); err != nil {
		return false, err
	}
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := recoverAbandonedWindowsTransition(); err != nil {
		return false, err
	}
	old, err := s.Config()
	if err != nil {
		return false, err
	}
	_, ok := u.serviceExecutable(old)
	if !ok {
		return false, fmt.Errorf("refusing to replace an unrelated Windows service")
	}
	status, err := s.Query()
	if err != nil {
		return false, err
	}
	if status.State != svc.Stopped && status.State != svc.Running {
		return false, fmt.Errorf("service is transitioning; retry update")
	}
	if _, err := os.Stat(windowsTransitionPath()); err == nil {
		return false, fmt.Errorf("an earlier update requires recovery")
	} else if !os.IsNotExist(err) {
		return false, err
	}
	// The helper waits on the lifecycle lock held here. Record its exact
	// process identity before releasing that lock so the monitor cannot mistake
	// a live activation for an abandoned transaction.
	cmd := exec.Command(exe, "update-activate")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	pid := uint32(cmd.Process.Pid)
	created, err := windowsProcessCreation(pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return false, err
	}
	_ = cmd.Process.Release()
	tx := windowsUpdateTransition{OldCommand: old.BinaryPathName, NewCommand: windows.ComposeCommandLine([]string{exe, "run", "-managed"}), WasRunning: status.State == svc.Running, StartType: old.StartType, HelperPID: pid, HelperCreated: created}
	if err := saveWindowsTransition(tx); err != nil {
		return true, err
	}
	if err := writeWindowsUpdateResult("Windows update has been handed to the activation helper; its result is pending."); err != nil {
		return true, err
	}
	return true, nil
}

func setServiceCommand(s *mgr.Service, command string) error {
	path, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return err
	}
	return windows.ChangeServiceConfig(s.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, path, nil, nil, nil, nil, nil, nil)
}
