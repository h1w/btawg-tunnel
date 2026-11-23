package e2e

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
)

func TestCarrierExperimentsDeliverAWGDatagrams(t *testing.T) {
	for _, tc := range []struct {
		mode, ext, req string
		pinless        bool
	}{
		{mode: config.ModeExtTCP},
		{mode: config.ModeExtUTP},
		{mode: config.ModeReqTCP},
		{mode: config.ModeReqUTP},
		{mode: config.ModeExtTCP, pinless: true},
		{mode: config.ModeExtTCP, ext: "protected"},
		{mode: config.ModeExtTCP, ext: "packed"},
		{mode: config.ModeExtUTP, ext: "padded"},
		{mode: config.ModeReqTCP, req: "window"},
		{mode: config.ModeReqUTP, req: "varied"},
	} {
		name := tc.ext + tc.req
		if name == "" {
			name = "default"
		}
		if tc.pinless {
			name = "legacy-import"
		}
		t.Run(tc.mode+"/"+name, func(t *testing.T) {
			t.Setenv("BTAWG_EXT_EXPERIMENT", tc.ext)
			t.Setenv("BTAWG_REQ_EXPERIMENT", tc.req)
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			relay := newRelay(t)
			hub, _, rows := hubWithClients(t, ctx, relay, []clientSpec{{id: "experiment", ip: "10.77.0.2"}}, tc.mode)
			cc := clientConfigFor(hub, rows[0], relay, rows[0].IP)
			if tc.pinless {
				cc.TLSServerKey = ""
			}
			hash, err := config.CarrierHash(hub.InfoHash(), hub.Config().LegacyTransport, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			cc.InfoHash = hash
			cc.CarrierEndpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(hub.Config().CarrierPort))
			c, err := client.New(cc)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)
			go c.Run(ctx)
			listener, err := hub.OverlayNet().ListenTCPAddrPort(netip.MustParseAddrPort("10.77.0.1:0"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					_, _ = io.Copy(conn, conn)
				}
			}()
			conn, err := c.OverlayDialTCP(ctx, listener.Addr().(*net.TCPAddr).AddrPort())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("authenticated overlay round trip\n"), 8192)
			wrote := make(chan error, 1)
			go func() { _, err := io.Copy(conn, bytes.NewReader(payload)); wrote <- err }()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatal(err)
			}
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("overlay payload changed")
			}
		})
	}
}
