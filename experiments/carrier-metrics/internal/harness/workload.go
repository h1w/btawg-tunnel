package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// WorkloadConfig controls one measurement run.
type WorkloadConfig struct {
	Mode                   string        // carrier mode name
	Experiment             string        // baseline, req-window, req-varied, ext-protected, ext-packed, ext-padded
	DelayMS                int           // one-way delay applied to the loopback path
	Repeat                 int           // repetition index
	SmallCount             int           // small round trips on the primary connection
	SmallPayload           int           // bytes per small round trip
	BulkBytes              int           // bytes for bulk echo on the primary connection
	ConcurrentSmallCount   int           // small round trips during bulk (0 disables)
	ConcurrentSmallPayload int           // bytes per concurrent small round trip
	BulkDeadline           time.Duration // max time for bulk echo
	SmallDeadline          time.Duration // max time for small round trips
	ConnectTimeout         time.Duration // max time for overlay dial
}

// Result holds all measurements from one run.
type Result struct {
	Mode       string  `json:"mode"`
	Experiment string  `json:"experiment"`
	DelayMS    int     `json:"delay_ms"`
	Repeat     int     `json:"repeat"`
	Error      string  `json:"error,omitempty"`
	ConnectedS float64 `json:"connected_s"`

	// Small round trips.
	SmallSent     int       `json:"small_sent"`
	SmallReceived int       `json:"small_received"`
	SmallBad      int       `json:"small_bad"`
	FirstEchoMS   float64   `json:"first_echo_ms"`
	SmallP50MS    float64   `json:"small_p50_ms"`
	SmallP95MS    float64   `json:"small_p95_ms"`
	SmallP99MS    float64   `json:"small_p99_ms"`
	SmallMaxMS    float64   `json:"small_max_ms"`
	SmallRTTMS    []float64 `json:"small_rtt_ms,omitempty"`

	// Concurrent small round trips during bulk.
	ConcurrentSmallSent     int       `json:"concurrent_small_sent"`
	ConcurrentSmallReceived int       `json:"concurrent_small_received"`
	ConcurrentSmallBad      int       `json:"concurrent_small_bad"`
	ConcurrentSmallP50MS    float64   `json:"concurrent_small_p50_ms"`
	ConcurrentSmallP95MS    float64   `json:"concurrent_small_p95_ms"`
	ConcurrentSmallP99MS    float64   `json:"concurrent_small_p99_ms"`
	ConcurrentSmallMaxMS    float64   `json:"concurrent_small_max_ms"`
	ConcurrentSmallRTTMS    []float64 `json:"concurrent_small_rtt_ms,omitempty"`

	// Bulk echo.
	BulkBytes      int     `json:"bulk_bytes"`
	BulkDurationS  float64 `json:"bulk_duration_s"`
	BulkThroughput float64 `json:"bulk_mbps"`
	BulkError      string  `json:"bulk_error,omitempty"`

	// Outer traffic counters from the hub (inner IP bytes).
	HubUpBytes   uint64 `json:"hub_up_bytes"`
	HubDownBytes uint64 `json:"hub_down_bytes"`

	// Optional pcap path.
	Pcap string `json:"pcap,omitempty"`
}

// RunOverlayWorkload opens overlay TCP sockets from A to B, runs small
// echoed round trips on one connection and (optionally) concurrent small
// round trips on a second connection while a bulk echo runs on the first.
// It verifies every byte.
func RunOverlayWorkload(ctx context.Context, h *Hub, cfg WorkloadConfig) (*Result, error) {
	res := &Result{
		Mode:       cfg.Mode,
		Experiment: cfg.Experiment,
		DelayMS:    cfg.DelayMS,
		Repeat:     cfg.Repeat,
	}

	// Ensure both carriers are attached to the hub.
	if err := h.WaitConnected(ctx, h.A.IP); err != nil {
		return res, fmt.Errorf("A not connected: %w", err)
	}
	if err := h.WaitConnected(ctx, h.B.IP); err != nil {
		return res, fmt.Errorf("B not connected: %w", err)
	}

	start := time.Now()

	// B listens; we will accept up to two connections.
	ln, err := h.B.Cl.OverlayListenTCP(9001)
	if err != nil {
		return res, fmt.Errorf("B listen: %w", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 2)
	acceptErr := make(chan error, 1)
	go func() {
		for range 2 {
			c, err := ln.Accept()
			if err != nil {
				select {
				case acceptErr <- err:
				default:
				}
				return
			}
			go copyEcho(c)
			accepted <- c
		}
	}()

	dial := func() (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
		defer cancel()
		return h.A.Cl.OverlayDialTCP(dialCtx, netip.MustParseAddrPort(h.B.IP+":9001"))
	}

	mainConn, err := dial()
	if err != nil {
		return res, fmt.Errorf("A dial main: %w", err)
	}
	defer mainConn.Close()

	// Prime the return path with one authenticated packet before measuring.
	if err := primeReturnPath(ctx, h, &h.A); err != nil {
		return res, fmt.Errorf("prime return path: %w", err)
	}

	res.ConnectedS = time.Since(start).Seconds()

	var concurrentConn net.Conn
	if cfg.ConcurrentSmallCount > 0 {
		concurrentConn, err = dial()
		if err != nil {
			return res, fmt.Errorf("A dial concurrent: %w", err)
		}
		defer concurrentConn.Close()
	}

	// Wait until B has accepted both connections.
	need := 1
	if concurrentConn != nil {
		need = 2
	}
	for range need {
		select {
		case <-accepted:
		case err := <-acceptErr:
			return res, fmt.Errorf("B accept: %w", err)
		case <-time.After(cfg.ConnectTimeout):
			return res, fmt.Errorf("B accept timeout")
		}
	}

	// Run primary small round trips.
	rtt, firstMS, err := smallRoundTrips(mainConn, cfg.SmallCount, cfg.SmallPayload, cfg.SmallDeadline)
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	res.SmallRTTMS = rtt
	res.SmallSent = cfg.SmallCount
	res.SmallReceived = len(rtt)
	res.SmallBad = cfg.SmallCount - len(rtt)
	res.FirstEchoMS = firstMS
	if len(rtt) > 0 {
		sort.Float64s(rtt)
		res.SmallP50MS = percentile(rtt, 0.50)
		res.SmallP95MS = percentile(rtt, 0.95)
		res.SmallP99MS = percentile(rtt, 0.99)
		res.SmallMaxMS = percentile(rtt, 1.0)
	}

	// Run bulk + concurrent small round trips.
	var wg sync.WaitGroup
	var bulkErr error
	var concurrentRTT []float64
	var concurrentErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		bulkStart := time.Now()
		if err := bulkEcho(mainConn, cfg.BulkBytes, cfg.BulkDeadline); err != nil {
			bulkErr = err
			return
		}
		res.BulkBytes = cfg.BulkBytes
		res.BulkDurationS = time.Since(bulkStart).Seconds()
		if res.BulkDurationS > 0 {
			res.BulkThroughput = float64(cfg.BulkBytes*8) / res.BulkDurationS / 1e6
		}
	}()

	if concurrentConn != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			concurrentRTT, _, concurrentErr = smallRoundTrips(concurrentConn, cfg.ConcurrentSmallCount,
				cfg.ConcurrentSmallPayload, cfg.BulkDeadline)
		}()
	}

	wg.Wait()
	if bulkErr != nil {
		res.BulkError = bulkErr.Error()
	}
	if concurrentErr != nil {
		res.Error = concurrentErr.Error()
	}
	if len(concurrentRTT) > 0 {
		res.ConcurrentSmallRTTMS = concurrentRTT
		res.ConcurrentSmallSent = cfg.ConcurrentSmallCount
		res.ConcurrentSmallReceived = len(concurrentRTT)
		res.ConcurrentSmallBad = cfg.ConcurrentSmallCount - len(concurrentRTT)
		sort.Float64s(concurrentRTT)
		res.ConcurrentSmallP50MS = percentile(concurrentRTT, 0.50)
		res.ConcurrentSmallP95MS = percentile(concurrentRTT, 0.95)
		res.ConcurrentSmallP99MS = percentile(concurrentRTT, 0.99)
		res.ConcurrentSmallMaxMS = percentile(concurrentRTT, 1.0)
	}

	// Collect hub traffic counters.
	tr := h.Srv.Traffic()
	if a, ok := tr[h.A.Rec.ID]; ok {
		res.HubUpBytes += a.Up
		res.HubDownBytes += a.Down
	}
	if b, ok := tr[h.B.Rec.ID]; ok {
		res.HubUpBytes += b.Up
		res.HubDownBytes += b.Down
	}

	return res, nil
}

func smallRoundTrips(c net.Conn, count, payload int, deadline time.Duration) ([]float64, float64, error) {
	if payload < 16 {
		payload = 16
	}
	send := make([]byte, payload)
	recv := make([]byte, payload)
	start := time.Now()
	c.SetDeadline(start.Add(deadline))
	defer c.SetDeadline(time.Time{})

	var firstMS float64
	rtt := make([]float64, 0, count)
	for i := range count {
		binary.BigEndian.PutUint64(send[0:], uint64(i))
		binary.BigEndian.PutUint64(send[8:], uint64(time.Since(start)))
		tx := time.Now()
		if _, err := c.Write(send); err != nil {
			return rtt, firstMS, fmt.Errorf("write %d: %w", i, err)
		}
		if _, err := io.ReadFull(c, recv); err != nil {
			return rtt, firstMS, fmt.Errorf("read %d: %w", i, err)
		}
		if !bytes.Equal(send, recv) {
			return rtt, firstMS, fmt.Errorf("mismatch at %d", i)
		}
		d := float64(time.Since(tx)) / float64(time.Millisecond)
		if i == 0 {
			firstMS = d
		}
		rtt = append(rtt, d)
	}
	return rtt, firstMS, nil
}

func bulkEcho(c net.Conn, size int, deadline time.Duration) error {
	send := make([]byte, size)
	for i := range send {
		send[i] = byte(i)
	}
	recv := make([]byte, size)
	tx := time.Now()
	c.SetDeadline(tx.Add(deadline))
	defer c.SetDeadline(time.Time{})
	if _, err := c.Write(send); err != nil {
		return fmt.Errorf("bulk write: %w", err)
	}
	if _, err := io.ReadFull(c, recv); err != nil {
		return fmt.Errorf("bulk read: %w", err)
	}
	if !bytes.Equal(send, recv) {
		return fmt.Errorf("bulk mismatch")
	}
	return nil
}

// copyEcho reads from c and writes back to c until EOF or error.
func copyEcho(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 65536)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		if _, err := c.Write(buf[:n]); err != nil {
			return
		}
	}
}

func primeReturnPath(ctx context.Context, h *Hub, ci *ClientInfo) error {
	ln, err := h.Srv.OverlayNet().ListenTCPAddrPort(netip.MustParseAddrPort("10.77.0.1:0"))
	if err != nil {
		return err
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr).AddrPort()

	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		close(done)
	}()

	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		dialCtx, cancel := context.WithTimeout(ctx, 450*time.Millisecond)
		c, err := ci.Cl.OverlayDialTCP(dialCtx, addr)
		cancel()
		if err == nil {
			c.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("prime timeout")
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return -1
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
