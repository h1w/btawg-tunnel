package carrier

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestAcceptDispatchExtended(t *testing.T) {
	client, server := tcpPair(t)
	hashes := map[[20]byte]string{testHash: "ext"}

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := Accept(server, peerB, hashes)
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	cli, pid, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("client extended: %v", err)
	}
	t.Cleanup(cli.Wait)
	if pid != peerB {
		t.Fatalf("client saw server peer ID %v, want %v", pid, peerB)
	}

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	t.Cleanup(r.tr.Wait)
	if r.pid != peerA {
		t.Fatalf("server saw peer ID %v, want %v", r.pid, peerA)
	}

	roundTrip(t, cli, r.tr)
}

func TestExtendedHandshakeAdvertisesNoProductName(t *testing.T) {
	for _, dialer := range []bool{true, false} {
		client, peer := tcpPair(t)
		if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		ready := make(chan result, 1)
		go func() {
			if dialer {
				tr, pid, err := NewExtended(client, testHash, peerA)
				ready <- result{tr: tr, pid: pid, err: err}
				return
			}
			tr, pid, _, err := Accept(client, peerA, map[[20]byte]string{testHash: "ext"})
			ready <- result{tr: tr, pid: pid, err: err}
		}()
		if _, err := Handshake(peer, testHash, peerB); err != nil {
			t.Fatal(err)
		}
		var length [4]byte
		if _, err := io.ReadFull(peer, length[:]); err != nil {
			t.Fatal(err)
		}
		n := binary.BigEndian.Uint32(length[:])
		if n < 2 || n > 128 {
			t.Fatalf("invalid extension handshake length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(peer, body); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.Write([]byte("\x00\x00\x00\x12\x14\x00d1:md5:btawgi1ee")); err != nil {
			t.Fatal(err)
		}
		r := <-ready
		if r.err != nil {
			t.Fatalf("legacy peer handshake: %v", r.err)
		}
		t.Cleanup(r.tr.Wait)
		// Legacy peers keep ID 1; the outbound handshake uses the opaque
		// infohash-derived name and correct bencode (closing both dicts).
		if bytes.Contains(body, []byte("btawg")) || body[0] != 20 || body[1] != 0 || !bytes.HasSuffix(body, []byte("eee")) {
			t.Fatalf("unexpected extension advertisement: %q", body)
		}
	}
}

func TestAcceptDispatchRequestPiece(t *testing.T) {
	client, server := tcpPair(t)
	hashes := map[[20]byte]string{testHash: "req"}

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := Accept(server, peerB, hashes)
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	// The server side of Accept advertises the odd half of the index space,
	// so the connecting client must advertise the even half.
	cli, pid, err := NewRequestPiece(client, testHash, peerA, false)
	if err != nil {
		t.Fatalf("client request/piece: %v", err)
	}
	t.Cleanup(cli.Wait)
	if pid != peerB {
		t.Fatalf("client saw server peer ID %v, want %v", pid, peerB)
	}

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	t.Cleanup(r.tr.Wait)
	if r.pid != peerA {
		t.Fatalf("server saw peer ID %v, want %v", r.pid, peerA)
	}

	roundTrip(t, cli, r.tr)
}

func TestAcceptRejectsUnknownHash(t *testing.T) {
	client, server := tcpPair(t)
	hashes := map[[20]byte]string{testHash: "ext"}

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := Accept(server, peerB, hashes)
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	unknown := testHash
	unknown[0] ^= 0xff
	if _, err := client.Write(HandshakeBytes(unknown, peerA)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	select {
	case r := <-srvCh:
		if r.err == nil {
			t.Fatal("accept with unknown infohash succeeded")
		}
		if r.tr != nil {
			t.Fatal("accept returned a transport for unknown infohash")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept did not reject unknown infohash")
	}
}

func TestAcceptDataTransfer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		client func(net.Conn) (Transport, [20]byte, error)
	}{
		{
			name: "extended",
			mode: "ext",
			client: func(c net.Conn) (Transport, [20]byte, error) {
				return NewExtended(c, testHash, peerA)
			},
		},
		{
			name: "request-piece",
			mode: "req",
			client: func(c net.Conn) (Transport, [20]byte, error) {
				return NewRequestPiece(c, testHash, peerA, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := tcpPair(t)
			hashes := map[[20]byte]string{testHash: tc.mode}

			srvCh := make(chan result, 1)
			go func() {
				tr, pid, _, err := Accept(server, peerB, hashes)
				srvCh <- result{tr: tr, pid: pid, err: err}
			}()

			cli, _, err := tc.client(client)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			t.Cleanup(cli.Wait)

			r := <-srvCh
			if r.err != nil {
				t.Fatalf("accept: %v", r.err)
			}
			t.Cleanup(r.tr.Wait)

			const count = 100
			for i := range count {
				payload := make([]byte, 64+i)
				if _, err := rand.Read(payload); err != nil {
					t.Fatal(err)
				}
				if err := cli.Send(payload); err != nil {
					t.Fatalf("send %d: %v", i, err)
				}
				select {
				case got := <-r.tr.Recv():
					if !bytes.Equal(got, payload) {
						t.Fatalf("payload %d mismatch", i)
					}
				case <-r.tr.Done():
					t.Fatalf("carrier closed at payload %d", i)
				case <-time.After(5 * time.Second):
					t.Fatalf("timeout at payload %d", i)
				}
			}
		})
	}
}

func TestAcceptDoesNotSetDeadline(t *testing.T) {
	client, server := tcpPair(t)
	hashes := map[[20]byte]string{testHash: "ext"}

	if err := server.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := Accept(server, peerB, hashes)
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	// Deliberately do not send anything from the client. Accept must fail with
	// the caller-set deadline, not hang.
	select {
	case r := <-srvCh:
		if r.err == nil {
			t.Fatal("accept succeeded without client handshake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accept ignored caller-set deadline")
	}
	_ = client
}
