package carrier

import (
	"net"
	"sync"
	"sync/atomic"
)

func (x *extCarrier) sendIdle(waiting *atomic.Int32) {
	if err := sendIdleFrame(x.c, &x.writeMu, waiting); err != nil {
		x.Close()
	}
}

func (x *requestPieceCarrier) sendIdle(waiting *atomic.Int32) {
	if err := sendIdleFrame(x.c, &x.writeMu, waiting); err != nil {
		x.Close()
	}
}

func sendIdleFrame(c net.Conn, mu *sync.Mutex, waiting *atomic.Int32) error {
	if waiting.Load() != 0 || !mu.TryLock() {
		return nil
	}
	defer mu.Unlock()
	if waiting.Load() != 0 {
		return nil
	}
	var frame [4]byte
	return fullWrite(c, frame[:])
}
