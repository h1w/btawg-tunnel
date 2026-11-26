package client

import (
	"context"
	"net"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
)

func TestCarrierStartsCryptoHandshakeOnAttachedStream(t *testing.T) {
	const deadline = 3 * time.Second // Less than the AWG retransmit interval.
	cfg := managedCfg(t)
	cfg.Transport = config.ModeReqTCP
	cfg.Profile.PersistentKeepalive = "25"
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg.CarrierEndpoint = ln.Addr().String()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx, stop := context.WithTimeout(t.Context(), deadline)
	defer stop()
	region, err := awg.InfoHashBytes(cfg.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.SetDeadline(time.Now().Add(deadline))
	finished := make(chan error, 1)
	go func() { finished <- c.runTunnel(ctx, nil) }()
	defer func() { stop(); <-finished }()
	raw, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(deadline))
	remote, _, err := carrier.NewRequestPiece(raw, region, [20]byte{42}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Wait()
	select {
	case packet := <-remote.Recv():
		if len(packet) == 0 {
			t.Fatal("empty datagram on attached carrier")
		}
	case <-ctx.Done():
		t.Fatal("AWG did not send its first handshake until the retry timer")
	}
}
