package awgrepair

import (
	"bytes"
	"encoding/binary"
	"time"
)

const (
	replicaWindow    = 25 * time.Millisecond
	maxReplicaPacket = 1280 // ordinary TUN MTU
	replicaSlots     = 128  // power of two for mask, inline buffers
)

const (
	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04

	ipMinHeaderBytes  = 20
	tcpMinHeaderBytes = 20
)

// eligibleReplica reports whether packet is a complete, non-fragmented IPv4
// TCP packet carrying data, suitable for enqueueing extra ordinary copies.
func eligibleReplica(packet []byte) bool {
	n := len(packet)
	if n < ipMinHeaderBytes || n > maxReplicaPacket {
		return false
	}
	versionIHL := packet[0]
	if versionIHL>>4 != 4 {
		return false
	}
	ihl := int(versionIHL&0x0f) * 4
	if ihl < ipMinHeaderBytes || n < ihl {
		return false
	}
	if int(binary.BigEndian.Uint16(packet[2:4])) != n {
		return false
	}
	// Reject any fragment: MF or any fragment offset.
	flagsFrag := binary.BigEndian.Uint16(packet[6:8])
	if flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0 {
		return false
	}
	if packet[9] != 6 { // TCP protocol number
		return false
	}
	if n < ihl+tcpMinHeaderBytes {
		return false
	}
	tcpOff := int(packet[ihl+12]>>4) * 4
	if tcpOff < tcpMinHeaderBytes || ihl+tcpOff > n {
		return false
	}
	tcpFlags := packet[ihl+13]
	if tcpFlags&(tcpFlagSYN|tcpFlagFIN|tcpFlagRST) != 0 {
		return false
	}
	return n > ihl+tcpOff
}

// replicaSlot stores one cached packet identity inline. The full cache is one
// contiguous allocation; there is no per-slot allocation.
type replicaSlot struct {
	id   uint64
	ts   time.Time
	len  uint16
	data [maxReplicaPacket]byte
}

// replicaCache is a direct-mapped, bounded cache of recently forwarded packets.
// A returned value of true means the packet is an ordinary duplicate that has
// already been forwarded in the current window and should be suppressed.
type replicaCache struct {
	slots [replicaSlots]replicaSlot
}

// duplicate records this packet and reports whether it is a duplicate of one
// forwarded within replicaWindow. The first occurrence is forwarded (false);
// identical packets inside the window are suppressed (true). Bytes are
// verified exactly before suppression, and a repeat does not extend the stored
// expiry time. The cache is direct-mapped, so collisions cause safe false
// negatives (eviction).
func (c *replicaCache) duplicate(packet []byte, now time.Time) bool {
	n := len(packet)
	if n == 0 || n > maxReplicaPacket {
		return false
	}
	id := packetID(packet)
	i := id & (replicaSlots - 1)
	s := &c.slots[i]
	if s.len != 0 && now.Sub(s.ts) < replicaWindow && s.len == uint16(n) && s.id == id && bytes.Equal(s.data[:n], packet) {
		return true
	}
	s.id = id
	s.ts = now
	s.len = uint16(n)
	copy(s.data[:], packet)
	return false
}
