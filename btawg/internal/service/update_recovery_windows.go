package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	waitOwnerGone() error
	installRecoveryMonitor(*windowsUpdateTransition) error
	isRunning() bool
	start() error
}

type windowsSCMTransition struct {
	service *mgr.Service
	owner   windowsServiceOwner
}

func (s *windowsSCMTransition) selected() (string, error) {
	c, err := s.service.Config()
	return c.BinaryPathName, err
}
func (s *windowsSCMTransition) selectCommand(command string) error {
	return setServiceCommand(s.service, command)
}
func (s *windowsSCMTransition) stop() error {
	owner, err := captureWindowsServiceOwner(s.service)
	if err != nil {
		return err
	}
	s.owner = owner
	return stopServiceControl(s.service)
}
func (s *windowsSCMTransition) waitOwnerGone() error {
	return waitWindowsServiceOwnerGone(s.owner)
}

func (s *windowsSCMTransition) isRunning() bool {
	status, err := s.service.Query()
	return err == nil && status.State == svc.Running
}

// The stopped client no longer blocks stopping the dependent recovery
// service. Replace the live monitor before selecting/starting the candidate.
func (s *windowsSCMTransition) installRecoveryMonitor(tx *windowsUpdateTransition) error {
	args, err := windows.DecomposeCommandLine(tx.NewCommand)
	if err != nil || len(args) != 3 || args[1] != "run" || args[2] != "-managed" {
		return errors.New("invalid staged update command")
	}
	executable := args[0]
	want := windows.ComposeCommandLine([]string{executable, "update-recovery"})
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
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
	trusted, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	if err != nil || len(trusted) != 2 || trusted[1] != "update-recovery" || !strings.EqualFold(cfg.ServiceStartName, "LocalSystem") || cfg.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS || cfg.StartType != mgr.StartAutomatic {
		return errors.New("refusing unrelated update recovery service")
	}
	if _, ok := Client.serviceExecutable(mgr.Config{BinaryPathName: windows.ComposeCommandLine([]string{trusted[0], "run", "-managed"}), ServiceStartName: cfg.ServiceStartName, ServiceType: cfg.ServiceType}); !ok {
		return errors.New("refusing unrelated update recovery image")
	}
	alreadyRunning := false
	if status, queryErr := recovery.Query(); queryErr == nil && status.State == svc.Running && status.ProcessId != 0 {
		image, imageErr := windowsProcessImage(status.ProcessId)
		alreadyRunning = imageErr == nil && strings.EqualFold(image, executable)
	}
	if strings.EqualFold(cfg.BinaryPathName, want) && alreadyRunning {
		return nil
	}
	if err := stopServiceControl(recovery); err != nil {
		return err
	}
	if err := setServiceCommand(recovery, want); err != nil {
		return err
	}
	if err := startService(recovery); err != nil {
		return err
	}
	status, err := recovery.Query()
	if err != nil || status.State != svc.Running || status.ProcessId == 0 {
		return errors.New("candidate recovery monitor is not running")
	}
	image, err := windowsProcessImage(status.ProcessId)
	if err != nil || !strings.EqualFold(image, executable) {
		return errors.New("candidate recovery monitor image mismatch")
	}
	return nil
}

func (s *windowsSCMTransition) start() error { return startService(s.service) }

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
		if err := s.waitOwnerGone(); err != nil {
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
		if !s.isRunning() {
			if err := s.waitOwnerGone(); err != nil {
				return err
			}
		}
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
	if err := s.waitOwnerGone(); err != nil {
		return err
	}
	if err := s.installRecoveryMonitor(&tx); err != nil {
		if tx.WasRunning {
			return errors.Join(err, s.start())
		}
		return err
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
		if err := s.waitOwnerGone(); err != nil {
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

func windowsProcessImage(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
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

type windowsServiceOwner struct {
	PID     uint32
	Created int64
}

func windowsManagedTUNSettings() (name string, enabled bool) {
	name, enabled = "btawg0", true
	data, err := os.ReadFile(filepath.Join(ManagedDataDirectory(), "client.json"))
	if err != nil {
		return name, enabled
	}
	var cfg struct {
		TUNName string `json:"tun_name"`
		NoTUN   bool   `json:"no_tun"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return name, enabled
	}
	if cfg.TUNName != "" {
		name = cfg.TUNName
	}
	return name, !cfg.NoTUN
}

func windowsManagedTUNName() string {
	name, _ := windowsManagedTUNSettings()
	return name
}

// Wait for the actual process that SCM reported, never for an adapter inferred
// from its alias. The TUN creator owns adapter retirement and startup checks.
func captureWindowsServiceOwner(s *mgr.Service) (windowsServiceOwner, error) {
	var owner windowsServiceOwner
	status, err := s.Query()
	if err != nil {
		return owner, err
	}
	owner.PID = status.ProcessId
	if owner.PID != 0 {
		created, err := windowsProcessCreation(owner.PID)
		if err == nil {
			owner.Created = created
		}
	}
	return owner, nil
}

func waitWindowsServiceOwnerGone(owner windowsServiceOwner) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		processAlive := false
		if owner.PID != 0 && owner.Created != 0 {
			created, err := windowsProcessCreation(owner.PID)
			processAlive = err == nil && created == owner.Created
			if err != nil && !errors.Is(err, errHelperExited) && !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				processAlive = true
			}
		}
		if !processAlive {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("previous service process still present (pid=%d): %w", owner.PID, ctx.Err())
		case <-ticker.C:
		}
	}
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

// At monitor startup, restore the command before reporting Running but keep
// the journal if the dependent client must start. The next tick finishes the
// restart after SCM can satisfy that dependency (also for manual services).
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
	if err := rollbackWindowsTransition(&windowsSCMTransition{service: s}, tx, restart); err != nil {
		return tx, err
	}
	if !restart && tx.WasRunning {
		return tx, writeWindowsUpdateResult("Windows update was interrupted; restoring the previous bundle...")
	}
	if err := writeWindowsUpdateResult("Windows update was interrupted and the previous bundle was restored."); err != nil {
		return tx, err
	}
	if err := os.Remove(windowsTransitionPath()); err != nil {
		return tx, err
	}
	return tx, nil
}

func RunWindowsUpdateActivation(prepare func() error) error {
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
	if prepare != nil {
		if err := prepare(); err != nil {
			// No SCM mutations have occurred; leave the old service as it was.
			if resultErr := writeWindowsUpdateResult("Windows update failed during preparation; previous bundle remains selected: " + err.Error()); resultErr != nil {
				return errors.Join(err, resultErr)
			}
			return errors.Join(err, os.Remove(windowsTransitionPath()))
		}
	}
	err = activateWindowsTransition(&windowsSCMTransition{service: s}, tx)
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
	want := windows.ComposeCommandLine([]string{executable, "update-recovery"})
	wantValid := mgr.Config{BinaryPathName: windows.ComposeCommandLine([]string{executable, "run", "-managed"}), ServiceStartName: "LocalSystem", ServiceType: windows.SERVICE_WIN32_OWN_PROCESS}
	if _, ok := Client.serviceExecutable(wantValid); !ok {
		return errors.New("invalid update recovery executable")
	}
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
	if !strings.EqualFold(cfg.BinaryPathName, want) {
		if err := setServiceCommand(recovery, want); err != nil {
			return err
		}
		cfg, err = recovery.Config()
		if err != nil || !strings.EqualFold(cfg.BinaryPathName, want) {
			return errors.New("SCM did not select verified update recovery executable")
		}
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
	tx, err := loadWindowsTransition()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, 1
	}
	if err != nil || !windowsHelperAlive(tx) {
		unlock, err := windowsLifecycleLock(context.Background())
		if err != nil {
			return true, 1
		}
		_, err = recoverWindowsTransition(false)
		unlock()
		if err != nil {
			return true, 1
		}
	}
	// A live activation helper holds lifecycle.lock. Acknowledge Running
	// without taking that lock or touching its journal; otherwise cutover
	// would deadlock or be mistaken for an abandoned update.
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
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
