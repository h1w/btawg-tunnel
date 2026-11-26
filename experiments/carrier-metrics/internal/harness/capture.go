package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Capture runs tcpdump in the background and writes a pcap file.
type Capture struct {
	path string
	cmd  *exec.Cmd
}

// StartCapture attempts to start tcpdump on the given interface.
// It returns nil without error if tcpdump is not available or cannot be run.
// toolsDir may point to /tmp/.../tools/usr/bin from the lab environment.
func StartCapture(iface, outDir, toolsDir string) (*Capture, error) {
	bin := findBinary("tcpdump", toolsDir)
	if bin == "" {
		return nil, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%d.pcap", iface, time.Now().Unix())
	path := filepath.Join(outDir, name)
	cmd := exec.Command(bin, "-i", iface, "-w", path, "-s", "0", "-nn", "-q")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Give tcpdump time to bind the BPF filter.
	time.Sleep(300 * time.Millisecond)
	return &Capture{path: path, cmd: cmd}, nil
}

// Path returns the pcap file path.
func (c *Capture) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Stop terminates tcpdump and returns the written path.
func (c *Capture) Stop() string {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return ""
	}
	_ = c.cmd.Process.Signal(os.Interrupt)
	_ = c.cmd.Wait()
	return c.path
}

func findBinary(name, toolsDir string) string {
	for _, dir := range []string{toolsDir, "/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}
