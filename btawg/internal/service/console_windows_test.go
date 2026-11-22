package service

import (
	"os"
	"regexp"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/eventlog"
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

func TestWindowsServiceLogsShowStartupError(t *testing.T) {
	if os.Getenv("BTAWG_WINDOWS_EVENTLOG_TESTS") != "1" {
		t.Skip("requires the btawg-client event source on a disposable Windows fixture")
	}
	logger, err := eventlog.Open("btawg-client")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	const message = "btawg-log-test: listen tcp4 127.0.0.1:1080: bind: address already in use <socket> & details"
	if err := logger.Error(1, message); err != nil {
		t.Fatal(err)
	}
	screen, err := Client.logs()
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\S+ ` + regexp.QuoteMeta(message) + `$`)
	if !want.MatchString(screen.Text) {
		t.Fatal("logs did not show the timestamp and complete service error")
	}
}
