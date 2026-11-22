package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

func (u Unit) updateLockPath() string { return u.binaryPath() + ".update.lock" }

func (u Unit) applyRelease(ctx context.Context, client *http.Client, tag string, progress func(string)) error {
	base := assetBase + tag + "/"
	name := u.binaryName()
	progress("Fetching SHA-256 checksum…")
	want, err := releaseChecksum(ctx, client, base, name)
	if err != nil {
		return fmt.Errorf("get release checksum: %w", err)
	}

	path := u.binaryPath()
	dir, err := os.MkdirTemp(filepath.Dir(path), ".btawg-update-")
	if err != nil {
		return fmt.Errorf("stage update: %w", err)
	}
	keepBackup := false
	defer func() {
		if !keepBackup {
			os.RemoveAll(dir)
		}
	}()
	staged := filepath.Join(dir, name)
	if err := downloadBinary(ctx, client, base+name, staged, want, progress); err != nil {
		return fmt.Errorf("download %s %s: %w", name, tag, err)
	}
	backup := filepath.Join(dir, "previous")
	if err := os.Link(path, backup); err != nil {
		return fmt.Errorf("back up installed binary: %w", err)
	}
	// Cancellation is safe up to this point. Once the executable is replaced,
	// finish the restart or rollback even if the dashboard is closing.
	if err := ctx.Err(); err != nil {
		return err
	}
	progress("Installing verified binary…")
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("replace installed binary: %w", err)
	}
	progress("Restarting service if running…")
	if err := u.Control("try-restart"); err != nil {
		progress("Restoring previous version…")
		if restoreErr := os.Rename(backup, path); restoreErr != nil {
			keepBackup = true
			return errors.Join(err, fmt.Errorf("restore previous binary (backup at %s): %w", backup, restoreErr))
		}
		if restartErr := u.Control("restart"); restartErr != nil {
			return errors.Join(err, fmt.Errorf("previous binary restored, but service restart failed: %w", restartErr))
		}
		return fmt.Errorf("previous version restored after restart failed: %w", err)
	}
	return nil
}

func platformUpdateNotice() string { return "" }
