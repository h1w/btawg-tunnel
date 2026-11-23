package carrier

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	idleInterval    = 5 * time.Second
	paceChunkBytes  = 8 << 10
	paceChunkPeriod = 2 * time.Millisecond
)

type timingOptions struct {
	bytesPerSecond int64
	idleCount      int
	idleInterval   time.Duration // zero uses the default
}
type idleTransport interface{ sendIdle(*atomic.Int32) }

// WithTiming applies optional hub-path bulk pacing and a finite keepalive
// budget. base serializes keepalives with BitTorrent messages; t owns data.
func WithTiming(t, base Transport) (Transport, error) {
	var o timingOptions
	if raw := os.Getenv("BTAWG_CARRIER_PACE_MBPS"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || n > 10000 {
			return nil, fmt.Errorf("BTAWG_CARRIER_PACE_MBPS must be in 0..10000")
		}
		o.bytesPerSecond = int64(n) * 125000
	}
	if raw := os.Getenv("BTAWG_CARRIER_IDLE_KEEPALIVES"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 8)
		if err != nil || n > 12 {
			return nil, fmt.Errorf("BTAWG_CARRIER_IDLE_KEEPALIVES must be in 0..12")
		}
		o.idleCount = int(n)
	}
	if o.bytesPerSecond == 0 && o.idleCount == 0 {
		return t, nil
	}
	if _, ok := base.(idleTransport); o.idleCount != 0 && !ok {
		return nil, fmt.Errorf("carrier does not support idle keepalives")
	}
	return newTimedTransport(t, base, o), nil
}

type timedTransport struct {
	Transport
	base     Transport
	opts     timingOptions
	waiting  atomic.Int32
	activity atomic.Int64
	// Operation order is reserved without holding the underlying write lock
	// during pacing, so request credits and other control traffic can flow.
	turn     chan struct{}
	idleDone chan struct{}
}

func newTimedTransport(t, base Transport, o timingOptions) *timedTransport {
	x := &timedTransport{Transport: t, base: base, opts: o, turn: make(chan struct{}, 1), idleDone: make(chan struct{})}
	x.turn <- struct{}{}
	x.activity.Store(time.Now().UnixNano())
	if o.idleCount != 0 {
		go x.idle()
	} else {
		close(x.idleDone)
	}
	return x
}

func (x *timedTransport) Send(p []byte) error { return x.SendBatch([][]byte{p}) }

func (x *timedTransport) SendBatch(packets [][]byte) error {
	if len(packets) == 0 {
		return nil
	}
	for _, p := range packets {
		if len(p) == 0 || len(p) > BlockSize-2 {
			return fmt.Errorf("invalid datagram size %d", len(p))
		}
	}
	x.waiting.Add(1)
	defer func() { x.activity.Store(time.Now().UnixNano()); x.waiting.Add(-1) }()
	if err := x.acquire(); err != nil {
		return err
	}
	defer func() { x.turn <- struct{}{} }()
	limit := min(int64(paceChunkBytes), x.opts.bytesPerSecond*int64(paceChunkPeriod)/int64(time.Second))
	for len(packets) > 0 {
		n, size := 1, len(packets[0])
		if x.opts.bytesPerSecond == 0 {
			n = len(packets)
		} else {
			for n < len(packets) && int64(size+len(packets[n])) <= limit {
				size += len(packets[n])
				n++
			}
		}
		started := time.Now()
		if err := sendBatch(x.Transport, packets[:n]); err != nil {
			return err
		}
		packets = packets[n:]
		if x.opts.bytesPerSecond > 0 && (len(packets) > 0 || x.waiting.Load() > 1) {
			target := started.Add(time.Duration(int64(size) * int64(time.Second) / x.opts.bytesPerSecond))
			if delay := time.Until(target); delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-t.C:
				case <-x.Done():
					t.Stop()
					return net.ErrClosed
				}
			}
		}
	}
	return nil
}

func (x *timedTransport) acquire() error {
	select {
	case <-x.Done():
		return net.ErrClosed
	case <-x.turn:
		select {
		case <-x.Done():
			x.turn <- struct{}{}
			return net.ErrClosed
		default:
			return nil
		}
	}
}

func sendBatch(t Transport, packets [][]byte) error {
	if b, ok := t.(interface{ SendBatch([][]byte) error }); ok && len(packets) > 1 {
		return b.SendBatch(packets)
	}
	for _, p := range packets {
		if err := t.Send(p); err != nil {
			return err
		}
	}
	return nil
}

func (x *timedTransport) idle() {
	defer close(x.idleDone)
	interval := x.opts.idleInterval
	if interval == 0 {
		interval = idleInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	out := x.base.(idleTransport)
	for range x.opts.idleCount {
		select {
		case <-x.Done():
			return
		case now := <-t.C:
			if x.waiting.Load() == 0 && now.Sub(time.Unix(0, x.activity.Load())) >= interval {
				out.sendIdle(&x.waiting)
			}
		}
	}
}

func (x *timedTransport) Wait() { x.Transport.Wait(); <-x.idleDone }
