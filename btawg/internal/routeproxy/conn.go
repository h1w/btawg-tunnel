package routeproxy

import (
	"context"
	"errors"
	"io"
	"net"
)

// BindConn closes conn when the overlay path's lifetime ends. Bind to the
// established path, not the context used only to dial it.
func BindConn(life context.Context, conn net.Conn) net.Conn {
	stop := closeOnCancel(life, conn)
	if life.Err() != nil {
		_ = conn.Close()
	}
	return &boundConn{Conn: conn, stop: stop}
}

type boundConn struct {
	net.Conn
	stop func() bool
}

func (c *boundConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func (c *boundConn) CloseWrite() error { return closeWrite(c.Conn) }

func closeWrite(c net.Conn) error {
	if tcp, ok := c.(interface{ CloseWrite() error }); ok {
		return tcp.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (c *boundConn) ReadFrom(r io.Reader) (int64, error) { return io.Copy(c.Conn, r) }
func (c *boundConn) WriteTo(w io.Writer) (int64, error)  { return io.Copy(w, c.Conn) }
