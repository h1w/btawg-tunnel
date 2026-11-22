package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func (u Unit) binaryName() string { return "btawg-" + string(u) }
func (u Unit) unitName() string   { return u.binaryName() }
func (u Unit) binaryPath() string {
	if m, err := mgr.Connect(); err == nil {
		defer m.Disconnect()
		if s, err := m.OpenService(u.unitName()); err == nil {
			defer s.Close()
			if cfg, err := s.Config(); err == nil {
				if exe, ok := u.serviceExecutable(cfg); ok {
					return exe
				}
			}
		}
	}
	return filepath.Join(windowsInstallDirectory(), u.binaryName()+".exe")
}

func windowsInstallDirectory() string {
	p, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFilesX64, 0)
	if err != nil {
		p = filepath.Join(windowsDirectoryVolume(), `\Program Files`)
	}
	return filepath.Join(p, "btawg")
}

func (u Unit) Install(extraArgs ...string) error {
	if u != Client || len(extraArgs) != 0 {
		return errors.New("only the managed Windows client can be installed")
	}
	if err := startWindowsRecoveryIfInstalled(); err != nil {
		return err
	}
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverAbandonedWindowsTransition(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if installed, err := m.OpenService(u.unitName()); err == nil {
		defer installed.Close()
		if err := u.verifyInstalledService(installed); err != nil {
			return err
		}
		return startService(installed)
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	if err := createManagedDirectory(windowsInstallDirectory()); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bundle, err := os.MkdirTemp(windowsInstallDirectory(), "bundle-")
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(bundle)
		}
	}()
	for _, name := range windowsBundleFiles {
		src := filepath.Join(filepath.Dir(exe), name)
		if name == "btawg-client.exe" {
			src = exe
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		err = writeFile(filepath.Join(bundle, name), f, 0o700)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if err := verifyWindowsDriver(filepath.Join(bundle, "wintun.dll")); err != nil {
		return err
	}
	s, err := m.CreateService(u.unitName(), filepath.Join(bundle, u.binaryName()+".exe"), mgr.Config{
		DisplayName: "btawg VPN client", Description: "btawg managed VPN client",
		StartType: mgr.StartAutomatic, ServiceStartName: "LocalSystem",
	}, "run", "-managed")
	if err != nil {
		return err
	}
	defer s.Close()
	if err := ensureWindowsEventSource(u.unitName()); err != nil {
		_ = s.Delete()
		return err
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 86400); err != nil {
		_ = s.Delete()
		return err
	}
	installed = true
	return startService(s)
}

func ensureWindowsEventSource(source string) error {
	const types = eventlog.Info | eventlog.Warning | eventlog.Error
	err := eventlog.InstallAsEventCreate(source, types)
	if err == nil {
		return nil
	}
	key, openErr := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\`+source, registry.QUERY_VALUE)
	if openErr != nil {
		return err
	}
	defer key.Close()
	file, _, fileErr := key.GetStringValue("EventMessageFile")
	got, _, typesErr := key.GetIntegerValue("TypesSupported")
	if fileErr != nil || typesErr != nil || !strings.EqualFold(file, `%SystemRoot%\System32\EventCreate.exe`) || got != uint64(types) {
		return err
	}
	return nil
}

func (u Unit) verifyInstalledService(s *mgr.Service) error {
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	if _, ok := u.serviceExecutable(cfg); !ok {
		return fmt.Errorf("refusing to manage unrelated Windows service %q", u.unitName())
	}
	return nil
}

func (u Unit) serviceExecutable(cfg mgr.Config) (string, bool) {
	args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	if err != nil || len(args) != 3 || args[1] != "run" || args[2] != "-managed" || !strings.EqualFold(cfg.ServiceStartName, "LocalSystem") {
		return "", false
	}
	exe := filepath.Clean(args[0])
	dir := filepath.Dir(exe)
	return exe, strings.EqualFold(filepath.Dir(dir), windowsInstallDirectory()) && strings.HasPrefix(filepath.Base(dir), "bundle-") &&
		strings.EqualFold(filepath.Base(exe), u.binaryName()+".exe") && cfg.ServiceType == windows.SERVICE_WIN32_OWN_PROCESS
}

func (u Unit) Control(action string) error {
	if err := startWindowsRecoveryIfInstalled(); err != nil {
		return err
	}
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverAbandonedWindowsTransition(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(u.unitName())
	if err != nil {
		return err
	}
	defer s.Close()
	if err := u.verifyInstalledService(s); err != nil {
		return err
	}
	switch action {
	case "start":
		return startService(s)
	case "stop":
		return stopService(s)
	case "restart", "try-restart":
		status, err := s.Query()
		if err != nil || action == "try-restart" && status.State == svc.Stopped {
			return err
		}
		if err := stopService(s); err != nil {
			return err
		}
		return startService(s)
	case "enable", "disable":
		start := uint32(mgr.StartManual)
		if action == "enable" {
			start = mgr.StartAutomatic
		}
		return windows.ChangeServiceConfig(s.Handle, windows.SERVICE_NO_CHANGE, start, windows.SERVICE_NO_CHANGE, nil, nil, nil, nil, nil, nil, nil)
	case "status":
		_, err := s.Query()
		return err
	default:
		return fmt.Errorf("unsupported action %q", action)
	}
}

func (u Unit) queryService() (svc.Status, uint32, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return svc.Status{}, 0, err
	}
	defer windows.CloseServiceHandle(h)
	name, _ := windows.UTF16PtrFromString(u.unitName())
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return svc.Status{}, 0, err
	}
	s := &mgr.Service{Name: u.unitName(), Handle: sh}
	defer s.Close()
	if err := u.verifyInstalledService(s); err != nil {
		return svc.Status{}, 0, err
	}
	status, err := s.Query()
	if err != nil {
		return status, 0, err
	}
	cfg, err := s.Config()
	return status, cfg.StartType, err
}

func (u Unit) State() (string, bool, error) {
	status, start, err := u.queryService()
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return NotInstalled, false, nil
	}
	if err != nil {
		return "", false, err
	}
	state := "inactive"
	switch status.State {
	case svc.Running:
		state = "active"
	case svc.StartPending, svc.ContinuePending:
		state = "activating"
	case svc.StopPending, svc.PausePending:
		state = "deactivating"
	case svc.Paused:
		state = "paused"
	case svc.Stopped:
		if status.Win32ExitCode != 0 {
			state = "failed"
		}
	}
	return state, start == mgr.StartAutomatic, nil
}

func (u Unit) ProcessID() (uint32, error) {
	status, _, err := u.queryService()
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return 0, nil
	}
	return status.ProcessId, err
}

func (u Unit) Uninstall() error {
	if err := startWindowsRecoveryIfInstalled(); err != nil {
		return err
	}
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverAbandonedWindowsTransition(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(u.unitName())
	if err != nil {
		return err
	}
	defer s.Close()
	if err := u.verifyInstalledService(s); err != nil {
		return err
	}
	if err := stopService(s); err != nil {
		return err
	}
	return s.Delete()
}

func startService(s *mgr.Service) error {
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitService(s, svc.Running)
}

func stopService(s *mgr.Service) error {
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) && !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
		return err
	}
	return waitService(s, svc.Stopped)
}

func waitService(s *mgr.Service, want svc.State) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := s.Query()
		if err != nil || status.State == want {
			return err
		}
		if want == svc.Running && status.State == svc.Stopped {
			return fmt.Errorf("Windows service stopped during startup (win32=%d, service=%d)", status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for service state %d (last state %d): %w", want, status.State, ctx.Err())
		case <-tick.C:
		}
	}
}
