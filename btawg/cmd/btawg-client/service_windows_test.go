package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/service"
)

func TestWindowsSCMLifecycle(t *testing.T) {
	binary := os.Getenv("BTAWG_WINDOWS_CLIENT_EXE")
	importFile := os.Getenv("BTAWG_WINDOWS_CLIENT_IMPORT_FILE")
	if binary == "" || importFile == "" {
		t.Skip("requires an isolated elevated fixture with an installer and test enrollment")
	}
	if got := windowsServiceOutput(t, "query", "btawg-client"); !strings.Contains(got, "1060") {
		t.Fatal("refusing to mutate an existing btawg-client service")
	}
	c := exec.CommandContext(t.Context(), binary, "install", "-import-file", importFile, "-no-tun")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("sc.exe", "stop", "btawg-client").Run()
		_ = exec.Command("sc.exe", "delete", "btawg-client").Run()
	})
	if got := windowsServiceOutput(t, "query", "btawg-client"); !strings.Contains(got, "RUNNING") {
		t.Fatalf("installed service not ready: %s", got)
	}
	status, running, err := client.RequestControl(client.ControlRequest{Op: "status"})
	if err != nil || !running || !status.OK || len(status.Enabled) == 0 {
		t.Fatalf("running service control: running=%v ok=%v enabled=%v err=%v", running, status.OK, status.Enabled, err)
	}
	if got := windowsServiceOutput(t, "qc", "btawg-client"); !strings.Contains(got, "AUTO_START") || strings.Contains(got, "btawg://") {
		t.Fatalf("unsafe or incorrect service configuration: %s", got)
	}
	if err := service.Client.Control("disable"); err != nil {
		t.Fatal(err)
	}
	if state, auto, err := service.Client.State(); err != nil || state != "active" || auto {
		t.Fatalf("autostart disable stopped service: %s, %v, %v", state, auto, err)
	}
	if err := service.Client.Control("stop"); err != nil {
		t.Fatal(err)
	}
	if got := windowsServiceOutput(t, "query", "btawg-client"); !strings.Contains(got, "STOPPED") {
		t.Fatalf("service did not stop: %s", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("ProgramData"), "btawg", "client.json")); err != nil {
		t.Fatalf("no persisted client settings: %v", err)
	}
}

func windowsServiceOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "sc.exe", args...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "1060") {
		t.Fatalf("sc %v: %v: %s", args, err, out)
	}
	return string(out)
}
