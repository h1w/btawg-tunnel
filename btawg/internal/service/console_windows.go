package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func (u Unit) logs() (*Screen, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := "*[System[Provider[@Name='" + u.unitName() + "']]]"
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "wevtutil.exe"), "qe", "Application", "/q:"+query, "/rd:true", "/c:50", "/f:xml", "/e:Events").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("read Windows Event Log: %w: %s", err, out)
	}
	var log struct {
		Events []struct {
			Created struct {
				Time string `xml:"SystemTime,attr"`
			} `xml:"System>TimeCreated"`
			Data []string `xml:"EventData>Data"`
		} `xml:"Event"`
	}
	if len(out) != 0 {
		if err := xml.Unmarshal(out, &log); err != nil {
			return nil, fmt.Errorf("decode Windows Event Log: %w", err)
		}
	}
	var text strings.Builder
	for i := len(log.Events) - 1; i >= 0; i-- {
		e := log.Events[i]
		if e.Created.Time != "" {
			text.WriteString(e.Created.Time)
			text.WriteByte(' ')
		}
		text.WriteString(strings.TrimSpace(strings.Join(e.Data, " ")))
		text.WriteByte('\n')
	}
	return &Screen{Title: "Service logs (last 50 entries)", Text: strings.TrimSpace(text.String())}, nil
}

func prepareDashboardConsole() (func(), error) {
	h := windows.Handle(os.Stdout.Fd())
	var previous uint32
	if err := windows.GetConsoleMode(h, &previous); err != nil {
		return nil, fmt.Errorf("open Windows terminal output: %w", err)
	}
	if err := windows.SetConsoleMode(h, previous|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, fmt.Errorf("enable Windows terminal output: %w", err)
	}
	return func() { _ = windows.SetConsoleMode(h, previous) }, nil
}
