package service

import (
	"errors"
	"testing"
)

type recoveryTestService struct {
	command   string
	running   bool
	failStart bool
}

func (s *recoveryTestService) selected() (string, error)          { return s.command, nil }
func (s *recoveryTestService) selectCommand(command string) error { s.command = command; return nil }
func (s *recoveryTestService) stop() error                        { s.running = false; return nil }
func (s *recoveryTestService) start() error {
	if s.failStart {
		s.failStart = false
		return errors.New("new bundle failed readiness")
	}
	s.running = true
	return nil
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
