package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tui "github.com/grindlemire/go-tui"
)

func (u Unit) logs() (*Screen, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", u.unitName(), "-n", "50", "--no-pager", "-o", "short-iso").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &Screen{Title: "Service logs (last 50 entries)", Text: strings.TrimSpace(string(out))}, nil
}

func prepareDashboardConsole() (func(), error) { return func() {}, nil }

func (d *dashboard) clipboardKey(tui.KeyEvent) bool { return false }
func (d *dashboard) clipboardHint() string          { return "" }
