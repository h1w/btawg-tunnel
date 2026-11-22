package client

import (
	"encoding/binary"
	"slices"
	"sync"
	"time"
)

const overlayFragmentLifetime = 30 * time.Second

// Keep bounded per-ingress IPv4 assemblies. A datagram is passed to peer
// policy and socket ownership only after all its bytes are available.
type overlayReassembly struct {
	mu    sync.Mutex
	items map[fragmentID]*overlayAssembly
	next  time.Time
	bytes int
}

type overlayAssembly struct {
	until  time.Time
	header []byte
	parts  []overlayFragment
	total  int
	bytes  int
}

type overlayFragment struct {
	offset int
	data   []byte
}

func (r *overlayReassembly) push(p []byte, now time.Time) []byte {
	if len(p) < 20 || p[0]>>4 != 4 {
		return nil
	}
	flags := binary.BigEndian.Uint16(p[6:8])
	if flags&0x3fff == 0 {
		return p
	}
	h := int(p[0]&15) * 4
	limit := int(binary.BigEndian.Uint16(p[2:4]))
	offset, more := int(flags&0x1fff)*8, flags&0x2000 != 0
	if h < 20 || limit > len(p) || limit <= h || offset+limit-h > 65535-20 || more && (limit-h)%8 != 0 {
		return nil
	}
	data := p[h:limit]
	end := offset + len(data)
	key := fragmentID{[4]byte(p[12:16]), [4]byte(p[16:20]), binary.BigEndian.Uint16(p[4:6]), p[9]}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !now.Before(r.next) {
		for id, assembly := range r.items {
			if !now.Before(assembly.until) {
				r.remove(id, assembly)
			}
		}
		r.next = now.Add(time.Second)
	}
	a := r.items[key]
	if a == nil {
		if len(r.items) >= 256 {
			return nil
		}
		if r.items == nil {
			r.items = make(map[fragmentID]*overlayAssembly)
		}
		a = &overlayAssembly{until: now.Add(overlayFragmentLifetime), total: -1}
		r.items[key] = a
	}
	if !now.Before(a.until) || r.bytes+len(data)+h > 1<<20 || len(a.parts) >= 128 || a.total >= 0 && (end > a.total || !more && end != a.total) {
		r.remove(key, a)
		return nil
	}
	for _, part := range a.parts {
		if offset < part.offset+len(part.data) && part.offset < end || !more && part.offset+len(part.data) > end {
			r.remove(key, a)
			return nil
		}
	}
	if offset == 0 {
		a.header = slices.Clone(p[:h])
		a.bytes += h
		r.bytes += h
	}
	if !more {
		a.total = end
	}
	a.parts = append(a.parts, overlayFragment{offset, slices.Clone(data)})
	a.bytes += len(data)
	r.bytes += len(data)
	if a.header == nil || a.total < 0 || a.bytes-len(a.header) != a.total {
		return nil
	}
	if len(a.header)+a.total > 65535 {
		r.remove(key, a)
		return nil
	}
	out := make([]byte, len(a.header)+a.total)
	copy(out, a.header)
	for _, part := range a.parts {
		copy(out[len(a.header)+part.offset:], part.data)
	}
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	binary.BigEndian.PutUint16(out[6:8], flags&0x4000)
	binary.BigEndian.PutUint16(out[10:12], 0)
	var sum uint32
	for i := 0; i < len(a.header); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(out[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(out[10:12], ^uint16(sum))
	r.remove(key, a)
	return out
}

func (r *overlayReassembly) remove(key fragmentID, a *overlayAssembly) {
	r.bytes -= a.bytes
	delete(r.items, key)
}
