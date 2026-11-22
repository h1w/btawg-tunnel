package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const windowsRecoveryService = "btawg-client-update-recovery"

type windowsUpdateTransition struct {
	OldCommand    string `json:"old_command"`
	NewCommand    string `json:"new_command"`
	WasRunning    bool   `json:"was_running"`
	StartType     uint32 `json:"start_type"`
	HelperPID     uint32 `json:"helper_pid"`
	HelperCreated int64  `json:"helper_created"`
	Committed     bool   `json:"committed"`
}

type windowsTransitionService interface {
	selected() (string, error)
	selectCommand(string) error
	stop() error
	start() error
}

type windowsSCMTransition struct{ service *mgr.Service }

func (s windowsSCMTransition) selected() (string, error) {
	c, err := s.service.Config()
	return c.BinaryPathName, err
}
func (s windowsSCMTransition) selectCommand(command string) error {
	return setServiceCommand(s.service, command)
}
func (s windowsSCMTransition) stop() error  { return stopService(s.service) }
func (s windowsSCMTransition) start() error { return startService(s.service) }

func rollbackWindowsTransition(s windowsTransitionService, tx windowsUpdateTransition, restart bool) error {
	selected, err := s.selected()
	if err != nil {
		return err
	}
	if selected != tx.OldCommand && selected != tx.NewCommand {
		return errors.New("service selection changed outside update; refusing rollback")
	}
	if selected != tx.OldCommand {
		if err := s.stop(); err != nil {
			return err
		}
		if err := s.selectCommand(tx.OldCommand); err != nil {
			return err
		}
		selected, err = s.selected()
		if err != nil {
			return err
		}
		if selected != tx.OldCommand {
			return errors.New("SCM did not restore previous service command")
		}
	}
	if restart && tx.WasRunning {
		return s.start()
	}
	return nil
}

func activateWindowsTransition(s windowsTransitionService, tx windowsUpdateTransition) error {
	selected, err := s.selected()
	if err != nil {
		return err
	}
	if selected != tx.OldCommand {
		return errors.New("service selection changed before activation")
	}
	if tx.WasRunning {
		if err := s.stop(); err != nil {
			return err
		}
	}
	if err := s.selectCommand(tx.NewCommand); err != nil {
		return errors.Join(err, rollbackWindowsTransition(s, tx, true))
	}
	selected, err = s.selected()
	if err != nil || selected != tx.NewCommand {
		if err == nil {
			err = errors.New("SCM did not select staged service command")
		}
		return errors.Join(err, rollbackWindowsTransition(s, tx, true))
	}
	if err := s.start(); err != nil {
		return errors.Join(err, rollbackWindowsTransition(s, tx, true))
	}
	if !tx.WasRunning {
		if err := s.stop(); err != nil {
			return errors.Join(err, rollbackWindowsTransition(s, tx, true))
		}
	}
	return nil
}

func windowsTransitionPath() string {
	return filepath.Join(windowsInstallDirectory(), "update-transition.json")
}
func windowsUpdateResultPath() string {
	return filepath.Join(windowsInstallDirectory(), "update-result.txt")
}

func saveWindowsTransition(tx windowsUpdateTransition) error {
	data, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	return WriteManagedFile(windowsTransitionPath(), data)
}
func loadWindowsTransition() (windowsUpdateTransition, error) {
	var tx windowsUpdateTransition
	data, err := os.ReadFile(windowsTransitionPath())
	if err != nil {
		return tx, err
	}
	if err := json.Unmarshal(data, &tx); err != nil {
		return tx, err
	}
	for _, command := range []string{tx.OldCommand, tx.NewCommand} {
		cfg := mgr.Config{BinaryPathName: command, ServiceStartName: "LocalSystem", ServiceType: windows.SERVICE_WIN32_OWN_PROCESS}
		if _, ok := Client.serviceExecutable(cfg); !ok {
			return tx, errors.New("invalid update transition command")
		}
	}
	if tx.OldCommand == tx.NewCommand {
		return tx, errors.New("invalid update transition selection")
	}
	return tx, nil
}
func writeWindowsUpdateResult(text string) error {
	return WriteManagedFile(windowsUpdateResultPath(), []byte(text))
}

var errHelperExited = errors.New("update helper already exited")

func windowsProcessCreation(pid uint32) (int64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	if exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return 0, errHelperExited
	}
	return created.Nanoseconds(), nil
}
func windowsHelperAlive(tx windowsUpdateTransition) bool {
	if tx.HelperPID == 0 || tx.HelperCreated == 0 {
		return false
	}
	created, err := windowsProcessCreation(tx.HelperPID)
	if err != nil && !errors.Is(err, windows.ERROR_INVALID_PARAMETER) && !errors.Is(err, errHelperExited) {
		return true
	}
	return err == nil && created == tx.HelperCreated
}

func windowsLifecycleLock(ctx context.Context) (func(), error) {
	if err := createManagedDirectory(windowsInstallDirectory()); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(windowsInstallDirectory(), "lifecycle.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = lockUpdate(file); err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func recoverAbandonedWindowsTransition() error {
	tx, err := loadWindowsTransition()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if windowsHelperAlive(tx) {
		return errors.New("Windows update activation is in progress")
	}
	_, err = recoverWindowsTransition(true)
	return err
}

func startWindowsRecoveryIfInstalled() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsRecoveryService)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close()
	return startService(s)
}

// Recovery never removes bundles: the selected image or a running dashboard may hold them open.
func recoverWindowsTransition(restart bool) (windowsUpdateTransition, error) {
	tx, err := loadWindowsTransition()
	if errors.Is(err, os.ErrNotExist) {
		return tx, nil
	}
	if err != nil {
		return tx, err
	}
	m, err := mgr.Connect()
	if err != nil {
		return tx, err
	}
	defer m.Disconnect()
	if tx.Committed {
		if err := writeWindowsUpdateResult("Windows update completed successfully."); err != nil {
			return tx, err
		}
		return windowsUpdateTransition{}, os.Remove(windowsTransitionPath())
	}
	s, err := m.OpenService(Client.unitName())
	if err != nil {
		return tx, err
	}
	defer s.Close()
	if err := Client.verifyInstalledService(s); err != nil {
		return tx, err
	}
	if err := rollbackWindowsTransition(windowsSCMTransition{s}, tx, restart); err != nil {
		return tx, err
	}
	if err := writeWindowsUpdateResult("Windows update was interrupted and the previous bundle was restored."); err != nil {
		return tx, err
	}
	if err := os.Remove(windowsTransitionPath()); err != nil {
		return tx, err
	}
	return tx, nil
}

func RunWindowsUpdateActivation() error {
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := loadWindowsTransition()
	if err != nil {
		return err
	}
	created, err := windowsProcessCreation(uint32(os.Getpid()))
	if err != nil {
		return err
	}
	if tx.HelperPID != uint32(os.Getpid()) || tx.HelperCreated != created {
		return errors.New("update helper does not own transition")
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Client.unitName())
	if err != nil {
		return err
	}
	defer s.Close()
	if err := Client.verifyInstalledService(s); err != nil {
		return err
	}
	err = activateWindowsTransition(windowsSCMTransition{s}, tx)
	if err != nil {
		// Leave the record for the monitor if rollback was incomplete.
		if _, recoverErr := recoverWindowsTransition(true); recoverErr != nil {
			return errors.Join(err, recoverErr)
		}
		if resultErr := writeWindowsUpdateResult("Windows update failed; previous bundle restored: " + err.Error()); resultErr != nil {
			return errors.Join(err, resultErr)
		}
		return err
	}
	tx.Committed = true
	if err := saveWindowsTransition(tx); err != nil {
		return err
	}
	if err := writeWindowsUpdateResult("Windows update completed successfully."); err != nil {
		return err
	}
	return os.Remove(windowsTransitionPath())
}

func ensureWindowsRecoveryService(m *mgr.Mgr, client *mgr.Service, executable string) error {
	recovery, err := m.OpenService(windowsRecoveryService)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		recovery, err = m.CreateService(windowsRecoveryService, executable, mgr.Config{DisplayName: "btawg update recovery", StartType: mgr.StartAutomatic, ServiceStartName: "LocalSystem"}, "update-recovery")
	}
	if err != nil {
		return err
	}
	defer recovery.Close()
	cfg, err := recovery.Config()
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	if err != nil || len(args) != 2 || args[1] != "update-recovery" || !strings.EqualFold(cfg.ServiceStartName, "LocalSystem") || cfg.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		return errors.New("refusing unrelated update recovery service")
	}
	valid := mgr.Config{BinaryPathName: windows.ComposeCommandLine([]string{args[0], "run", "-managed"}), ServiceStartName: "LocalSystem", ServiceType: windows.SERVICE_WIN32_OWN_PROCESS}
	if _, ok := Client.serviceExecutable(valid); !ok {
		return errors.New("invalid update recovery executable")
	}
	if cfg.StartType != mgr.StartAutomatic {
		return errors.New("update recovery service is not automatic")
	}
	clientCfg, err := client.Config()
	if err != nil {
		return err
	}
	deps := append([]string(nil), clientCfg.Dependencies...)
	found := false
	for _, dep := range deps {
		if strings.EqualFold(dep, windowsRecoveryService) {
			found = true
		}
	}
	if !found {
		deps = append(deps, windowsRecoveryService)
		var data []uint16
		for _, dep := range deps {
			encoded, err := windows.UTF16FromString(dep)
			if err != nil {
				return err
			}
			data = append(data, encoded...)
		}
		data = append(data, 0)
		if err := windows.ChangeServiceConfig(client.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, nil, nil, nil, &data[0], nil, nil, nil); err != nil {
			return err
		}
	}
	return startService(recovery)
}

// RunWindowsUpdateRecovery is entered only by SCM, via the protected installed image.
func RunWindowsUpdateRecovery() error {
	return svc.Run(windowsRecoveryService, windowsRecoveryHandler{})
}

type windowsRecoveryHandler struct{}

func (windowsRecoveryHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 120000}
	// No application lifecycle operation holds this lock while starting the
	// recovery service, so boot recovery can finish before client startup.
	unlock, err := windowsLifecycleLock(context.Background())
	if err != nil {
		return true, 1
	}
	tx, err := recoverWindowsTransition(false)
	unlock()
	if err != nil {
		return true, 1
	}
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	// A manually-started service was running before reboot; SCM will not restart it.
	if tx.WasRunning && tx.StartType != mgr.StartAutomatic {
		go func() {
			m, e := mgr.Connect()
			if e != nil {
				return
			}
			defer m.Disconnect()
			recovery, e := m.OpenService(windowsRecoveryService)
			if e != nil {
				return
			}
			defer recovery.Close()
			if waitService(recovery, svc.Running) != nil {
				return
			}
			unlock, e := windowsLifecycleLock(context.Background())
			if e != nil {
				return
			}
			defer unlock()
			s, e := m.OpenService(Client.unitName())
			if e != nil {
				return
			}
			defer s.Close()
			cfg, e := s.Config()
			if e != nil {
				return
			}
			status, e := s.Query()
			if e != nil {
				return
			}
			if cfg.BinaryPathName == tx.OldCommand && status.State == svc.Stopped && Client.verifyInstalledService(s) == nil {
				_ = startService(s)
			}
		}()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			case svc.Stop, svc.Shutdown:
				return false, 0
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			unlock, e := windowsLifecycleLock(ctx)
			cancel()
			if e == nil {
				tx, readErr := loadWindowsTransition()
				if readErr == nil && !windowsHelperAlive(tx) {
					_, _ = recoverWindowsTransition(true)
				}
				unlock()
			}
		}
	}
}

// WindowsUpdateNotice reports the last durable update outcome to the dashboard.
func WindowsUpdateNotice() string  { return windowsUpdateResult() }
func platformUpdateNotice() string { return windowsUpdateResult() }
func windowsUpdateResult() string {
	data, err := os.ReadFile(windowsUpdateResultPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
