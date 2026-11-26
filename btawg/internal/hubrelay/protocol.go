// Package hubrelay implements the native SOCKS client's TCP egress handshake.
package hubrelay

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"
)

const (
	Port    = 1058
	Timeout = 8 * time.Second
	version = 1
)

// Status is the outcome of a request on an established relay connection.
type Status byte

const (
	Ready Status = iota
	Denied
	Unreachable
)

func Request(ctx context.Context, c net.Conn, dst netip.AddrPort) error {
	if !dst.Addr().Is4() || dst.Port() == 0 {
		return errors.New("hub TCP relay requires an IPv4 destination and port")
	}
	deadline := time.Now().Add(Timeout)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := c.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	var b [7]byte
	b[0] = version
	ip := dst.Addr().As4()
	copy(b[1:5], ip[:])
	binary.BigEndian.PutUint16(b[5:], dst.Port())
	if _, err := c.Write(b[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, b[:1]); err != nil {
		return err
	}
	switch Status(b[0]) {
	case Ready:
		if !stop() || ctx.Err() != nil {
			return ctx.Err()
		}
		return c.SetDeadline(time.Time{})
	case Denied:
		return errors.New("hub TCP relay denied destination")
	default:
		return errors.New("hub TCP relay could not reach destination")
	}
}

func Read(r io.Reader) (netip.AddrPort, error) {
	var b [7]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return netip.AddrPort{}, err
	}
	port := binary.BigEndian.Uint16(b[5:])
	if b[0] != version || port == 0 {
		return netip.AddrPort{}, errors.New("invalid hub TCP relay request")
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[1:5])), port), nil
}

func Reply(w io.Writer, status Status) error {
	_, err := w.Write([]byte{byte(status)})
	return err
}
