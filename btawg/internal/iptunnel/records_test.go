package iptunnel

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/carrier"
)

func TestProtectedDatagramRecords(t *testing.T) {
	for _, mode := range []string{"protected", "packed", "padded"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			t.Cleanup(func() { a.Close(); b.Close() })
			for _, c := range []net.Conn{a, b} {
				if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := NewDatagramRecords(a, mode)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Wait()
			rx, err := NewDatagramRecords(b, mode)
			if err != nil {
				t.Fatal(err)
			}
			defer rx.Wait()
			batch := [][]byte{[]byte("interactive"), []byte("another"), bytes.Repeat([]byte{0x81}, carrier.BlockSize-2)}
			if err := tx.SendBatch(batch); err != nil {
				t.Fatal(err)
			}
			if err := tx.Send([]byte("no batch wait")); err != nil {
				t.Fatal(err)
			}
			for _, want := range append(batch, []byte("no batch wait")) {
				select {
				case got := <-rx.Recv():
					if !bytes.Equal(got, want) {
						t.Fatalf("received %d bytes, want %d", len(got), len(want))
					}
				case <-time.After(4 * time.Second):
					t.Fatal("delivery stalled")
				}
			}
		})
	}
}

func TestProtectedDatagramRecordRejectsTruncation(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	r, err := NewDatagramRecords(b, "packed")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Wait()
	done := make(chan error, 1)
	go func() { _, err := a.Write([]byte{0, 7, 0, 0, 0, 3, 'x'}); a.Close(); done <- err }()
	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("truncated record did not close carrier")
	}
	if err := <-done; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	select {
	case <-r.Recv():
		t.Fatal("truncated record delivered payload")
	default:
	}
}
