package client

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestOverlayReplySelector(t *testing.T) {
	local := [4]byte{10, 77, 0, 2}
	remote := [4]byte{203, 0, 113, 8}
	tcpReply := testOverlayPacket(6, remote, local, 443, 51000)
	udpReply := testOverlayPacket(17, remote, local, 53, 52000)
	quotedRequest := testOverlayPacket(6, local, remote, 51000, 443)
	icmpError := testOverlayPacket(1, [4]byte{10, 77, 0, 1}, local, 0, 0)
	icmpError[20] = 3
	icmpError = append(icmpError, quotedRequest...)
	tail := append([]byte(nil), tcpReply...)
	binary.BigEndian.PutUint16(tail[6:8], 1)
	wrongQuote := append([]byte(nil), icmpError...)
	wrongQuote[28+15]++

	tcpKey := localPort{port: 51000, proto: 6, remote: netip.MustParseAddrPort("203.0.113.8:443")}
	udpKey := localPort{port: 52000, proto: 17}
	for _, tc := range []struct {
		name string
		ip   []byte
		want localPort
	}{
		{"TCP reply", tcpReply, tcpKey},
		{"UDP association reply", udpReply, udpKey},
		{"ICMP quoted TCP request", icmpError, tcpKey},
		{"unrelated ICMP quotation", wrongQuote, localPort{}},
		{"truncated TCP", tcpReply[:22], localPort{}},
		{"fragment tail has no ports", tail, localPort{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlayReplyKey(tc.ip, local); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func testOverlayPacket(proto byte, src, dst [4]byte, sport, dport uint16) []byte {
	p := make([]byte, 28)
	p[0], p[9] = 0x45, proto
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	binary.BigEndian.PutUint16(p[20:22], sport)
	binary.BigEndian.PutUint16(p[22:24], dport)
	return p
}
