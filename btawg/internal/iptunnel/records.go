package iptunnel

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"bittorrent-tunnel/btawg/internal/carrier"
)

const (
	maxDatagram        = carrier.BlockSize - 2
	recordDataLimit    = 8 << 10
	recordPaddingLimit = 255
	recordBufferSize   = 4 + recordDataLimit + recordPaddingLimit
)

// DatagramRecords carries ordered datagrams over an authenticated stream.
// The caller owns authentication and must complete it before constructing records.
// Format: uint16 body length, uint16 padding length, then length-prefixed
// datagrams followed by padding. All metadata is inside the secure stream.
type DatagramRecords struct {
	stream   io.ReadWriteCloser
	packed   bool
	padded   bool
	mu       sync.Mutex
	write    [recordBufferSize]byte
	received chan []byte
	done     chan struct{}
	finished chan struct{}
	once     sync.Once
}

func NewDatagramRecords(stream io.ReadWriteCloser, mode string) (*DatagramRecords, error) {
	if mode != "protected" && mode != "packed" && mode != "padded" {
		return nil, fmt.Errorf("invalid datagram record mode %q", mode)
	}
	r := &DatagramRecords{stream: stream, packed: mode != "protected", padded: mode == "padded", received: make(chan []byte, 256), done: make(chan struct{}), finished: make(chan struct{})}
	go r.readLoop()
	return r, nil
}

// SecureDatagramRecords wraps an already authenticated TLS connection.
// Abort the carrier before waiting for readers instead of sending close_notify
// while the peer may not be reading.
func SecureDatagramRecords(secure *tls.Conn, base carrier.Transport, mode string) (*DatagramRecords, error) {
	return NewDatagramRecords(recordStream{Conn: secure, base: base}, mode)
}

type recordStream struct {
	net.Conn
	base carrier.Transport
}

func (s recordStream) Close() error {
	s.base.Wait()
	return nil
}

func (r *DatagramRecords) Send(p []byte) error { return r.SendBatch([][]byte{p}) }

func (r *DatagramRecords) SendBatch(batch [][]byte) error {
	for _, p := range batch {
		if len(p) == 0 || len(p) > maxDatagram {
			return fmt.Errorf("invalid datagram size %d", len(p))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(batch) != 0 {
		b := r.write[:]
		end, count := 4, 0
		for _, p := range batch {
			if end+2+len(p) > 4+recordDataLimit {
				break
			}
			binary.BigEndian.PutUint16(b[end:], uint16(len(p)))
			end += 2 + copy(b[end+2:], p)
			count++
			if !r.packed {
				break
			}
		}
		pad := 0
		if r.padded {
			var entropy [1]byte
			rand.Read(entropy[:])
			pad = int(entropy[0])
			clear(b[end : end+pad])
		}
		binary.BigEndian.PutUint16(b, uint16(end-4+pad))
		binary.BigEndian.PutUint16(b[2:], uint16(pad))
		if n, err := r.stream.Write(b[:end+pad]); err != nil || n != end+pad {
			r.Close()
			if err == nil {
				err = io.ErrShortWrite
			}
			return err
		}
		batch = batch[count:]
	}
	return nil
}

func (r *DatagramRecords) readLoop() {
	defer close(r.finished)
	defer r.Close()
	var buf [recordBufferSize - 4]byte
	var header [4]byte
	for {
		if _, err := io.ReadFull(r.stream, header[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(header[:2]))
		pad := int(binary.BigEndian.Uint16(header[2:]))
		if pad > recordPaddingLimit || !r.padded && pad != 0 || n-pad < 3 || n-pad > recordDataLimit {
			return
		}
		if _, err := io.ReadFull(r.stream, buf[:n]); err != nil {
			return
		}
		body := buf[:n-pad]
		// Validate the whole record before publishing any datagram.
		for rest := body; len(rest) > 0; {
			if len(rest) < 2 {
				return
			}
			n := int(binary.BigEndian.Uint16(rest))
			if n == 0 || n > maxDatagram || n+2 > len(rest) || !r.packed && n+2 != len(rest) {
				return
			}
			rest = rest[n+2:]
		}
		for len(body) > 0 {
			n := int(binary.BigEndian.Uint16(body))
			p := append([]byte(nil), body[2:n+2]...)
			select {
			case r.received <- p:
			case <-r.done:
				return
			}
			body = body[n+2:]
		}
	}
}

func (r *DatagramRecords) Recv() <-chan []byte   { return r.received }
func (r *DatagramRecords) Done() <-chan struct{} { return r.done }
func (r *DatagramRecords) Close()                { r.once.Do(func() { close(r.done); _ = r.stream.Close() }) }
func (r *DatagramRecords) Wait()                 { r.Close(); <-r.finished }
