package tunsetup

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	relayWANRTT    = 20 * time.Millisecond
	relayPaceTick  = 100 * time.Millisecond
	relayPaceFloor = 256 << 10 // bytes/s, not a traffic floor
	relayPaceStart = 512 << 10 // bytes acknowledged before capping BBR's probes
	relayMaxSend   = 4 << 20   // requested size; Linux accounts up to twice this
)

// Do not lock the socket's send buffer if kernel autotuning can already
// grow it at least as large, or if SO_SNDBUF cannot increase its ceiling.
func RelayWANSendBuffer() int {
	auto, err := os.ReadFile("/proc/sys/net/ipv4/tcp_wmem")
	if err != nil {
		return 0
	}
	core, err := os.ReadFile("/proc/sys/net/core/wmem_max")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(auto))
	if len(fields) != 3 {
		return 0
	}
	autoMax, err1 := strconv.Atoi(fields[2])
	coreMax, err2 := strconv.Atoi(strings.TrimSpace(string(core)))
	if err1 != nil || err2 != nil || autoMax <= 0 || coreMax <= 0 {
		return 0
	}
	want := min(relayMaxSend, coreMax)
	if 2*want <= autoMax {
		return 0
	}
	return want
}

type relayPacing struct {
	capRate uint64
	cut     time.Time
}

func (p *relayPacing) update(now time.Time, rtt, queue time.Duration, delivered uint64) uint64 {
	switch {
	case p.capRate == 0:
		p.capRate = max(relayPaceFloor, delivered*5/4)
	case queue > 12*time.Millisecond && now.Sub(p.cut) > rtt:
		p.capRate = max(relayPaceFloor, min(p.capRate*4/5, delivered*9/10))
		p.cut = now
	case queue < 7500*time.Microsecond && now.Sub(p.cut) > 2*rtt && delivered > p.capRate/2:
		p.capRate = max(p.capRate, min(p.capRate*5/4, delivered*3/2))
	}
	return p.capRate
}

// Only established WAN BBR relay sockets get a larger buffer or a sampler.
// Leave LAN sockets and kernels without BBR on their existing TCP policy.
func TuneRelayTCP(ctx context.Context, c *net.TCPConn, sendBuffer int) {
	tuneWANBBR(ctx, c, sendBuffer, false)
}

// TuneNativeUpload keeps the initial send buffer until TCP_INFO shows
// sustained send-buffer pressure on an established WAN BBR socket.
func TuneNativeUpload(c *net.TCPConn) {
	// The sampler exits when this socket closes; the dial context only
	// covers connection setup, not the lifetime of the SOCKS stream.
	tuneWANBBR(context.Background(), c, relayMaxSend, true)
}

func tuneWANBBR(ctx context.Context, c *net.TCPConn, sendBuffer int, waitForPressure bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	var wanBBR bool
	if err := raw.Control(func(fd uintptr) {
		s := int(fd)
		cc, err := unix.GetsockoptString(s, unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		if err != nil || cc != "bbr" {
			return
		}
		info, err := unix.GetsockoptTCPInfo(s, unix.IPPROTO_TCP, unix.TCP_INFO)
		wanBBR = err == nil && time.Duration(info.Min_rtt)*time.Microsecond >= relayWANRTT && info.Min_rtt <= info.Rtt
		if wanBBR && sendBuffer > 0 && !waitForPressure {
			_ = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, sendBuffer)
		}
	}); err != nil || !wanBBR {
		return
	}
	var pendingBuffer int
	if waitForPressure {
		pendingBuffer = sendBuffer
	}
	go paceRelayTCP(ctx, raw, pendingBuffer)
}

func paceRelayTCP(ctx context.Context, raw syscall.RawConn, pendingBuffer int) {
	setCap := func(rate uint64) bool {
		var optionErr error
		err := raw.Control(func(fd uintptr) {
			optionErr = unix.SetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE, rate)
		})
		return err == nil && optionErr == nil
	}
	tick := time.NewTicker(relayPaceTick)
	defer tick.Stop()
	var pacing relayPacing
	var prev time.Time
	var ack, prevLimited uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			var info *unix.TCPInfo
			if err := raw.Control(func(fd uintptr) {
				info, _ = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			}); err != nil || info == nil {
				return
			}
			if info.Min_rtt == 0 || info.Bytes_acked < relayPaceStart {
				continue
			}
			if info.Notsent_bytes == 0 && info.Unacked == 0 {
				if pacing.capRate != 0 && !setCap(^uint64(0)) {
					return
				}
				pacing = relayPacing{}
				prev = time.Time{}
				continue
			}
			rtt := time.Duration(info.Min_rtt) * time.Microsecond
			if prev.IsZero() {
				prev, ack, prevLimited = now, info.Bytes_acked, info.Sndbuf_limited
				continue
			}
			elapsed := now.Sub(prev)
			if elapsed < rtt {
				continue
			}
			delivered := (info.Bytes_acked - ack) * uint64(time.Second) / uint64(elapsed)
			limited := info.Sndbuf_limited - prevLimited
			prev, ack, prevLimited = now, info.Bytes_acked, info.Sndbuf_limited
			if delivered == 0 {
				continue
			}
			old := pacing.capRate
			queue := (time.Duration(info.Rtt) - time.Duration(info.Min_rtt)) * time.Microsecond
			if pendingBuffer > 0 {
				if queue >= 7500*time.Microsecond || limited < uint64(elapsed.Microseconds())/5 {
					continue
				}
				// Apply the pacing cap before widening the kernel queue.
				if !setCap(pacing.update(now, rtt, queue, delivered)) {
					return
				}
				var grew bool
				if err := raw.Control(func(fd uintptr) {
					s := int(fd)
					before, err := unix.GetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF)
					if err != nil || before >= 2*pendingBuffer || unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, pendingBuffer) != nil {
						return
					}
					after, err := unix.GetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF)
					grew = err == nil && after > before
				}); err != nil || !grew {
					setCap(^uint64(0))
					return
				}
				pendingBuffer = 0
				continue
			}
			if next := pacing.update(now, rtt, queue, delivered); next != old && !setCap(next) {
				return
			}
		}
	}
}
