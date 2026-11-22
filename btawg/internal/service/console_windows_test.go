package service

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDashboardEnablesVirtualTerminalOutput(t *testing.T) {
	h := windows.Handle(os.Stdout.Fd())
	var previous uint32
	if err := windows.GetConsoleMode(h, &previous); err != nil {
		t.Skip("requires a real Windows console")
	}
	defer windows.SetConsoleMode(h, previous)
	if err := windows.SetConsoleMode(h, previous&^windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		t.Fatal(err)
	}
	restore, err := prepareDashboardConsole()
	if err != nil {
		t.Fatal(err)
	}
	var active uint32
	if err := windows.GetConsoleMode(h, &active); err != nil {
		t.Fatal(err)
	}
	if active&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatal("real console would show escape codes instead of a dashboard")
	}
	restore()
	if err := windows.GetConsoleMode(h, &active); err != nil || active != previous&^windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING {
		t.Fatalf("console output mode not restored: %d %v", active, err)
	}
}
