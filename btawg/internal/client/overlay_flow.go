package client

import (
	"encoding/binary"
	"net/netip"
)

// overlayReplyKey finds the local source selector in a reply or an ICMP
// error quoting our outbound packet. Fragment tails have no socket selector.
func overlayReplyKey(p []byte, local [4]byte) localPort {
	h := ipv4FirstHeader(p)
	if h == 0 || [4]byte(p[16:20]) != local {
		return localPort{}
	}
	if p[9] == 1 {
		if len(p) < h+8 {
			return localPort{}
		}
		switch p[h] {
		case 3, 11, 12: // Destination Unreachable, Time Exceeded, Parameter Problem.
		default:
			return localPort{}
		}
		p = p[h+8:]
		h = ipv4FirstHeader(p)
		if h == 0 || [4]byte(p[12:16]) != local {
			return localPort{}
		}
		return overlayPort(p, h, 16, 0, 2)
	}
	return overlayPort(p, h, 12, 2, 0)
}

func ipv4FirstHeader(p []byte) int {
	if len(p) < 20 || p[0]>>4 != 4 || binary.BigEndian.Uint16(p[6:8])&0x1fff != 0 {
		return 0
	}
	h := int(p[0]&15) * 4
	if h < 20 || len(p) < h+4 {
		return 0
	}
	return h
}

func overlayPort(p []byte, h, remoteIP, localOffset, remoteOffset int) localPort {
	if p[9] != 6 && p[9] != 17 {
		return localPort{}
	}
	k := localPort{port: binary.BigEndian.Uint16(p[h+localOffset:]), proto: p[9]}
	if k.proto == 6 {
		k.remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[remoteIP:remoteIP+4])), binary.BigEndian.Uint16(p[h+remoteOffset:]))
	}
	return k
}
