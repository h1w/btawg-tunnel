package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// installPaths are overridable in tests.
var (
	unitDir = "/etc/systemd/system"
	binDir  = "/usr/local/bin"
)

// UnitDirs returns the current unit and binary directories. Exposed for cmd tests.
func UnitDirs() (string, string) { return unitDir, binDir }

// SetUnitDirs overrides the unit and binary directories. Exposed for cmd tests.
func SetUnitDirs(u, b string) { unitDir, binDir = u, b }

func (u Unit) binaryName() string { return "btawg-" + string(u) }
func (u Unit) unitName() string   { return u.binaryName() + ".service" }
func (u Unit) binaryPath() string { return filepath.Join(binDir, u.binaryName()) }
func (u Unit) unitPath() string   { return filepath.Join(unitDir, u.unitName()) }

// Install copies the running binary into /usr/local/bin and installs,
// enables and starts the systemd unit. Any extra args are appended to the
// unit's ExecStart command after the implicit `run -managed`. The import
// secret is never placed in unit arguments; callers must persist it to
// /etc/btawg/client.json before installing the client unit.
func (u Unit) Install(extraArgs ...string) error {
	const src = "/proc/self/exe"
	if err := copyFile(src, u.binaryPath(), 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	unit := u.renderUnit(extraArgs)
	if err := WriteFile(u.unitPath(), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := daemonReload(); err != nil {
		return err
	}
	if err := unitAction(u.unitName(), "enable"); err != nil {
		return err
	}
	return unitAction(u.unitName(), "restart")
}

// Control executes a systemd action for the unit. Supported actions are the
// same as systemctl: start, stop, restart, enable, disable, status.
func (u Unit) Control(action string) error {
	switch action {
	case "start", "stop", "restart", "try-restart", "enable", "disable":
		return unitAction(u.unitName(), action)
	case "status":
		_, _, err := u.State()
		return err
	default:
		return fmt.Errorf("unsupported action %q", action)
	}
}

// State returns the unit's ActiveState (or NotInstalled) and autostart state.
func (u Unit) State() (string, bool, error) {
	return unitStatus(u.unitName())
}

// Uninstall stops and disables our unit, leaving the launcher and all settings
// intact so the operator can reinstall without re-enrolling clients.
func (u Unit) Uninstall() error {
	if _, err := os.Lstat(u.unitPath()); err != nil {
		return fmt.Errorf("cannot remove unmanaged unit %s: %w", u.unitName(), err)
	}
	state, _, err := u.State()
	if err != nil {
		return err
	}
	if state != NotInstalled {
		if err := u.Control("stop"); err != nil {
			return err
		}
	}
	if err := u.Control("disable"); err != nil {
		return err
	}
	if err := os.Remove(u.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return daemonReload()
}

func (u Unit) renderUnit(extraArgs []string) string {
	exec := fmt.Sprintf("%s run -managed", u.binaryPath())
	for i, arg := range extraArgs {
		extraArgs[i] = strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(arg))
	}
	if len(extraArgs) > 0 {
		exec += " " + strings.Join(extraArgs, " ")
	}
	var extra string
	switch u {
	case Server:
		extra = "StateDirectory=btawg\n"
	case Client:
		extra = "RuntimeDirectory=btawg-client\n"
	}
	return fmt.Sprintf(`[Unit]
Description=btawg VPN %s
After=network.target

[Service]
Type=notify
ExecStart=%s
TimeoutStartSec=45
TimeoutStopSec=20
Restart=on-failure
RestartSec=5
%s
[Install]
WantedBy=multi-user.target
`, u, exec, extra)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeFile(dst, in, mode)
}
