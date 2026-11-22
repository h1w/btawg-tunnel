package carrier

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"bittorrent-tunnel/btawg/internal/tunsetup"

	"github.com/anacrolix/go-utp/purego"
)

func init() { purego.MatrixDisableNagle = true }

// NewUTPSocket prepares the same transport on both ends of the carrier.
// The kernel packet queues absorb bursts separately from the uTP flight window.
func NewUTPSocket(conn *net.UDPConn) (*purego.Socket, error) {
	var bytesPerSecond int64
	if value := os.Getenv("BTAWG_UTP_PACE_MBPS"); value != "" {
		rate, err := strconv.ParseInt(value, 10, 32)
		if err != nil || (rate != 0 && (rate < 50 || rate > 10000)) {
			return nil, fmt.Errorf("BTAWG_UTP_PACE_MBPS must be 0 (off) or an integer from 50 to 10000")
		}
		bytesPerSecond = rate * 125000
	}
	const packetQueue = 4 << 20
	if err := conn.SetReadBuffer(packetQueue); err != nil {
		return nil, fmt.Errorf("uTP UDP receive buffer: %w", err)
	}
	if err := conn.SetWriteBuffer(packetQueue); err != nil {
		return nil, fmt.Errorf("uTP UDP send buffer: %w", err)
	}
	// At 200 ms RTT, 500 Mbit/s of application data needs about 19 MB
	// in flight when request/piece transmits 2 KiB frames for 1.35 KiB payloads.
	const flightWindow = 24 << 20
	return purego.NewSocketFromPacketConn(tunsetup.ProtectUnderlayPacket(conn),
		purego.WithBufferSizes(flightWindow, flightWindow), purego.WithPacingRate(bytesPerSecond))
}
