package service

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type recoveryTestService struct {
	command     string
	running     bool
	failStart   bool
	monitorErr  error
	ownerGate   <-chan struct{}
	ownerErr    error
	ownerFailAt int
	ownerWaits  int
	started     chan struct{}
	startOnce   sync.Once
}

func (s *recoveryTestService) selected() (string, error)          { return s.command, nil }
func (s *recoveryTestService) selectCommand(command string) error { s.command = command; return nil }
func (s *recoveryTestService) stop() error                        { s.running = false; return nil }
func (s *recoveryTestService) waitOwnerGone() error {
	s.ownerWaits++
	if s.ownerFailAt != 0 && s.ownerWaits == s.ownerFailAt {
		return errors.New("candidate owner still present")
	}
	if s.ownerErr != nil {
		return s.ownerErr
	}
	if s.ownerGate != nil {
		<-s.ownerGate
	}
	return nil
}
func (s *recoveryTestService) start() error {
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.failStart {
		s.failStart = false
		return errors.New("new bundle failed readiness")
	}
	s.running = true
	return nil
}

func (s *recoveryTestService) isRunning() bool { return s.running }

func (s *recoveryTestService) installRecoveryMonitor(*windowsUpdateTransition) error {
	return s.monitorErr
}

func TestUpdateRollbackWaitsWhenCommandAlreadyRestored(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s := &recoveryTestService{command: "old", ownerGate: release, started: started}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	done := make(chan error, 1)
	go func() { done <- rollbackWindowsTransition(s, tx, true) }()
	select {
	case <-started:
		t.Fatal("previous bundle restarted before the stale owner was gone")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback did not resume after the stale owner was gone")
	}
	if s.ownerWaits == 0 {
		t.Fatal("rollback with restored command did not wait for the stale owner")
	}
	if !s.running {
		t.Fatal("previous bundle was not restarted")
	}
}

func TestUpdateActivationWaitsForFormerOwnerBeforeStart(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s := &recoveryTestService{command: "old", running: true, ownerGate: release, started: started}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	done := make(chan error, 1)
	go func() { done <- activateWindowsTransition(s, tx) }()
	select {
	case <-started:
		t.Fatal("new bundle started before the former owner was gone")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("activation did not resume after the former owner was gone")
	}
	if s.ownerWaits == 0 {
		t.Fatal("activation did not wait for the former owner")
	}
	if s.command != "new" || !s.running {
		t.Fatalf("activation left command %q, running %t", s.command, s.running)
	}
}

func TestUpdateRollbackWaitsForFormerOwnerBeforeRestart(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s := &recoveryTestService{command: "new", ownerGate: release, started: started}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	done := make(chan error, 1)
	go func() { done <- rollbackWindowsTransition(s, tx, true) }()
	select {
	case <-started:
		t.Fatal("previous bundle restarted before the former owner was gone")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback did not resume after the former owner was gone")
	}
	if s.ownerWaits == 0 {
		t.Fatal("rollback did not wait for the former owner")
	}
	if s.command != "old" || !s.running {
		t.Fatalf("rollback left command %q, running %t", s.command, s.running)
	}
}

func TestUpdateActivationWaitsForOwnerOfStoppedService(t *testing.T) {
	s := &recoveryTestService{command: "old", started: make(chan struct{})}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: false}
	if err := activateWindowsTransition(s, tx); err != nil {
		t.Fatal(err)
	}
	if s.ownerWaits == 0 {
		t.Fatal("activation of a stopped service did not wait for a stale owner")
	}
}

func TestUpdateActivationWaitsForStoppedCandidateTeardown(t *testing.T) {
	s := &recoveryTestService{command: "old", ownerFailAt: 2}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: false}
	if err := activateWindowsTransition(s, tx); err == nil {
		t.Fatal("stopped-service activation committed before candidate teardown completed")
	}
	if s.command != "old" {
		t.Fatalf("failed activation left command %q", s.command)
	}
	if s.ownerWaits < 2 {
		t.Fatalf("owner waits = %d, want teardown wait after readiness stop", s.ownerWaits)
	}
}

func TestUpdateActivationRefusesStaleOwner(t *testing.T) {
	s := &recoveryTestService{command: "old", running: true, ownerErr: errors.New("former adapter still present")}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	if err := activateWindowsTransition(s, tx); err == nil {
		t.Fatal("activation committed with a stale owner")
	}
	if s.command != "old" {
		t.Fatalf("stale owner left command %q", s.command)
	}
}

func TestUpdateActivationRestoresServiceWhenMonitorCutoverFails(t *testing.T) {
	s := &recoveryTestService{command: "old", running: false, monitorErr: errors.New("monitor cutover failed")}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	if err := activateWindowsTransition(s, tx); err == nil {
		t.Fatal("monitor cutover failure was ignored")
	}
	if s.command != "old" || !s.running {
		t.Fatalf("failed cutover left command %q, running %t", s.command, s.running)
	}
}

func TestUpdateRecoveryRestoresInterruptedSelection(t *testing.T) {
	s := &recoveryTestService{command: "new", running: false}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	if err := rollbackWindowsTransition(s, tx, true); err != nil {
		t.Fatal(err)
	}
	if s.command != "old" || !s.running {
		t.Fatalf("rollback left command %q, running %t", s.command, s.running)
	}
}

func TestUpdateActivationRollsBackFailedReadiness(t *testing.T) {
	s := &recoveryTestService{command: "old", running: true, failStart: true}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	if err := activateWindowsTransition(s, tx); err == nil {
		t.Fatal("failed readiness reported success")
	}
	if s.command != "old" || !s.running {
		t.Fatalf("failed activation left command %q, running %t", s.command, s.running)
	}
}

func TestUpdateRecoveryRefusesUnexpectedSelection(t *testing.T) {
	s := &recoveryTestService{command: "foreign", running: true}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: true}
	if err := rollbackWindowsTransition(s, tx, true); err == nil {
		t.Fatal("overwrote unexpected selection")
	}
	if s.command != "foreign" || !s.running {
		t.Fatalf("unexpected service was modified")
	}
}

func TestUpdateActivationProvesReadinessWithoutChangingStoppedState(t *testing.T) {
	s := &recoveryTestService{command: "old", running: false, failStart: true}
	tx := windowsUpdateTransition{OldCommand: "old", NewCommand: "new", WasRunning: false}
	if err := activateWindowsTransition(s, tx); err == nil {
		t.Fatal("unready bundle committed")
	}
	if s.command != "old" || s.running {
		t.Fatalf("rollback left command %q, running %t", s.command, s.running)
	}
}
