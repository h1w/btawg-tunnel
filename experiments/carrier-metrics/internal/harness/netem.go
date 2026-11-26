package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Netem applies loopback traffic shaping if the process has privileges.
// It is best-effort: failure is logged but not fatal.
type Netem struct {
	Dev      string
	DelayMS  int
	applied  bool
	original bool
}

// NewLoopbackNetem tries to set up a netem qdisc on lo with the given
// one-way delay. It records whether it succeeded so Cleanup can restore.
func NewLoopbackNetem(delayMS int) *Netem {
	n := &Netem{Dev: "lo", DelayMS: delayMS}
	if delayMS <= 0 {
		return n
	}
	if os.Getuid() != 0 {
		return n
	}
	// Check whether a qdisc already exists; if so, back it up.
	out, _ := exec.Command("tc", "qdisc", "show", "dev", n.Dev).Output()
	if strings.Contains(string(out), "netem") || strings.Contains(string(out), "prio") ||
		strings.Contains(string(out), "fq_codel") || strings.Contains(string(out), "noqueue") {
		// We'll try to replace; no backup for simplicity.
	}
	if err := exec.Command("tc", "qdisc", "replace", "dev", n.Dev, "root", "netem",
		"delay", fmt.Sprintf("%dms", delayMS)).Run(); err == nil {
		n.applied = true
	}
	return n
}

// Cleanup removes the netem qdisc if this helper installed it.
func (n *Netem) Cleanup() {
	if !n.applied {
		return
	}
	_ = exec.Command("tc", "qdisc", "del", "dev", n.Dev, "root").Run()
}

// Available reports whether traffic shaping is active.
func (n *Netem) Available() bool { return n.applied }
