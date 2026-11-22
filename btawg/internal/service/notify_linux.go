package service

import "github.com/coreos/go-systemd/v22/daemon"

// NotifyReady tells systemd that the daemon has finished initialization.
// It is a no-op when not running under systemd.
func NotifyReady() error {
	_, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	return err
}
