package harness

import (
	"context"
	"os"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
)

// TestSetExperimentEnv is a contract test: it verifies the harness sets the
// opt-in variables the btawg implementation is expected to read. It would
// fail if a refactor dropped one of the variables or used the wrong value.
func TestSetExperimentEnv(t *testing.T) {
	cases := []struct {
		exp, req, ext string
	}{
		{"baseline", "baseline", "baseline"},
		{"req-window", "window", "baseline"},
		{"req-varied", "varied", "baseline"},
		{"ext-protected", "baseline", "protected"},
		{"ext-packed", "baseline", "packed"},
		{"ext-padded", "baseline", "padded"},
	}
	for _, c := range cases {
		SetExperimentEnv(c.exp, 0, 0)
		if got := os.Getenv("BTAWG_REQ_EXPERIMENT"); got != c.req {
			t.Errorf("%s: BTAWG_REQ_EXPERIMENT=%q, want %q", c.exp, got, c.req)
		}
		if got := os.Getenv("BTAWG_EXT_EXPERIMENT"); got != c.ext {
			t.Errorf("%s: BTAWG_EXT_EXPERIMENT=%q, want %q", c.exp, got, c.ext)
		}
	}
}

// TestPaceAndIdleKeepalives verifies optional timing env knobs are bounded.
func TestPaceAndIdleKeepalives(t *testing.T) {
	SetExperimentEnv("baseline", 12, 20)
	if got := os.Getenv("BTAWG_CARRIER_PACE_MBPS"); got != "12" {
		t.Errorf("pace Mbps=%q, want 12", got)
	}
	if got := os.Getenv("BTAWG_CARRIER_IDLE_KEEPALIVES"); got != "12" {
		t.Errorf("idle keepalives=%q, want 12", got)
	}
}

// TestValidModesAreCarrier ensures the default mode list maps to supported
// carrier modes. This catches drift if a mode name is renamed or removed.
func TestValidModesAreCarrier(t *testing.T) {
	modes := []string{
		"ext-tcp", "ext-utp", "req-tcp", "req-utp",
		"tls-ext-tcp", "tls-ext-utp", "tls-req-tcp", "tls-req-utp",
	}
	for _, m := range modes {
		cm := config.CanonicalMode(m)
		if !config.IsCarrierMode(cm) {
			t.Errorf("%s canonicalized to %s, not a carrier mode", m, cm)
		}
	}
}

// TestHubStartsAndClientsAttach is an end-to-end contract test for the
// harness itself: it proves the harness can bring up a real hub and attach
// two registered clients. Without this, later measurements would silently
// time out in CI.
func TestHubStartsAndClientsAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hub, err := SetupHub(ctx, config.ModeExtTCP)
	if err != nil {
		t.Fatalf("SetupHub: %v", err)
	}
	defer hub.Close()

	if err := hub.StartClient(ctx, &hub.A); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err := hub.StartClient(ctx, &hub.B); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err := hub.WaitConnected(ctx, hub.A.IP); err != nil {
		t.Fatalf("A not connected: %v", err)
	}
	if err := hub.WaitConnected(ctx, hub.B.IP); err != nil {
		t.Fatalf("B not connected: %v", err)
	}
	if hub.A.Sock == "" || hub.B.Sock == "" {
		t.Fatalf("SOCKS5 addresses missing: A=%q B=%q", hub.A.Sock, hub.B.Sock)
	}
}
