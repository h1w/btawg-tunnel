package awgrepair

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

type pollableTestTUN struct {
	tun.Device
	f *os.File
}

func (p pollableTestTUN) Read(bufs [][]byte, sizes []int, off int) (int, error) {
	n, err := p.f.Read(bufs[0][off:])
	if n == 0 {
		return 0, err
	}
	sizes[0] = n
	return 1, err
}

func TestNativeReadWakeBeforeBlockAndResume(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	reader := &nativeReader{dev: pollableTestTUN{f: r}, file: r}
	defer reader.close()
	var data [16]byte
	bufs := [][]byte{data[:]}
	sizes := []int{0}
	reader.signal()
	done := make(chan error, 1)
	go func() {
		n, err := reader.read(bufs, sizes, 3)
		if n != 0 {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control offered before Read left an idle TUN blocked")
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	n, err := reader.read(bufs, sizes, 3)
	if err != nil || n != 1 || sizes[0] != 3 || string(data[3:6]) != "abc" {
		t.Fatalf("failed to resume data reads: n=%d err=%v data=%q", n, err, data)
	}
}
