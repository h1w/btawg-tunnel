package harness

import "os"

// SetExperimentEnv selects the requested lab variant. Explicit baseline keeps
// unrelated carrier families out of production auto-negotiation.
func SetExperimentEnv(exp string, paceMbps, idleKeepalives int) {
	os.Setenv("BTAWG_REQ_EXPERIMENT", "baseline")
	os.Setenv("BTAWG_EXT_EXPERIMENT", "baseline")
	os.Unsetenv("BTAWG_CARRIER_PACE_MBPS")
	os.Unsetenv("BTAWG_CARRIER_IDLE_KEEPALIVES")
	switch exp {
	case "req-window":
		os.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	case "req-varied":
		os.Setenv("BTAWG_REQ_EXPERIMENT", "varied")
	case "ext-protected":
		os.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	case "ext-packed":
		os.Setenv("BTAWG_EXT_EXPERIMENT", "packed")
	case "ext-padded":
		os.Setenv("BTAWG_EXT_EXPERIMENT", "padded")
	}
	if paceMbps > 0 {
		os.Setenv("BTAWG_CARRIER_PACE_MBPS", itoa(paceMbps))
	}
	if idleKeepalives > 0 {
		if idleKeepalives > 12 {
			idleKeepalives = 12
		}
		os.Setenv("BTAWG_CARRIER_IDLE_KEEPALIVES", itoa(idleKeepalives))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
