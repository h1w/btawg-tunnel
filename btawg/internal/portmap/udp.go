package portmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

type routerResult struct {
	protocol string
	code     uint16
}

func (r routerResult) Error() string { return fmt.Sprintf("%s result %d", r.protocol, r.code) }

func uncertainWrite(started time.Time, err error) lease {
	var result routerResult
	if errors.As(err, &result) {
		return lease{}
	}
	// A UDP write or HTTP action may reach the router even with no confirmed
	// reply. Do not delete or reuse the unconfirmed mapping; let it expire.
	return acknowledgedLease(netip.AddrPort{}, requestedSeconds, started, false)
}

// udpRequest opens a fresh, connected socket for each exchange: only the
// configured gateway IP and UDP source port can deliver a reply, and a
// delayed reply from a previous exchange cannot satisfy the next request.
func (r router) udpRequest(parent context.Context, request []byte, retransmit bool, match func([]byte) (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	dialer := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(r.source, 0)), Control: r.socketControl}
	conn, err := dialer.DialContext(ctx, "udp4", netip.AddrPortFrom(r.gateway, r.natPort).String())
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// PCP allows at most 1100 bytes. An extra byte detects oversized packets
	// even with a truncated read. NAT-PMP has fixed, much smaller payloads.
	var buf [1101]byte
	interval := 250 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.validateSnapshot(); err != nil {
			return err
		}
		if _, err := conn.Write(request); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		end := deadline
		if retransmit {
			end = minTime(deadline, time.Now().Add(interval))
			interval *= 2
		}
		if err := conn.SetReadDeadline(end); err != nil {
			return err
		}
		for {
			n, err := conn.Read(buf[:])
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !time.Now().Before(deadline) {
					return context.DeadlineExceeded
				}
				var ne net.Error
				if retransmit && errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if matched, err := match(buf[:n]); matched {
				return err
			}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

const (
	pcpVersion = 2
	pcpMap     = 1
	udpProto   = 17
)

type pcpMapping struct {
	router router
	port   uint16
	nonce  [12]byte
}

func pcpHeader(buf []byte, source netip.Addr, op byte, lifetime uint32) {
	buf[0], buf[1] = pcpVersion, op
	binary.BigEndian.PutUint32(buf[4:8], lifetime)
	addr := source.As16()
	copy(buf[8:24], addr[:])
}

func pcpResponse(buf []byte, op byte, base int) bool {
	if len(buf) < base || len(buf) > 1100 || len(buf)%4 != 0 || buf[0] != pcpVersion || buf[1] != 0x80|op {
		return false
	}
	for tail := buf[base:]; len(tail) > 0; {
		// No options were requested. Ignore optional, but not mandatory,
		// options we don't understand (e.g. an unexpected THIRD_PARTY).
		if tail[0] < 128 {
			return false
		}
		n := int(binary.BigEndian.Uint16(tail[2:4]))
		padded := (n + 3) &^ 3
		if len(tail) < 4+padded {
			return false
		}
		tail = tail[4+padded:]
	}
	return true
}

func (r router) probePCP(ctx context.Context, port uint16) (mapping, error) {
	var req [24]byte
	pcpHeader(req[:], r.source, 0, 0)
	err := r.udpRequest(ctx, req[:], true, func(buf []byte) (bool, error) {
		if len(buf) == 8 && buf[0] == 0 && (buf[1] == 0 || buf[1] == 0x80) && binary.BigEndian.Uint16(buf[2:4]) == 1 {
			return true, routerResult{"NAT-PMP", 1}
		}
		if !pcpResponse(buf, 0, 24) {
			return false, nil
		}
		if buf[3] != 0 {
			return true, routerResult{"PCP", uint16(buf[3])}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	m := &pcpMapping{router: r, port: port}
	if _, err := rand.Read(m.nonce[:]); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *pcpMapping) transact(ctx context.Context, seconds uint32, suggest netip.AddrPort) (netip.AddrPort, uint32, error) {
	var req [60]byte
	pcpHeader(req[:], m.router.source, pcpMap, seconds)
	copy(req[24:36], m.nonce[:])
	req[36] = udpProto
	binary.BigEndian.PutUint16(req[40:42], m.port)
	if seconds != 0 {
		ip, port := netip.IPv4Unspecified(), m.port
		if suggest.IsValid() {
			port = suggest.Port()
			if suggest.Addr().Is4() {
				ip = suggest.Addr()
			}
		}
		binary.BigEndian.PutUint16(req[42:44], port)
		mapped := ip.As16()
		copy(req[44:], mapped[:])
	}
	var endpoint netip.AddrPort
	var lifetime uint32
	err := m.router.udpRequest(ctx, req[:], false, func(buf []byte) (bool, error) {
		if !pcpResponse(buf, pcpMap, 60) || !bytes.Equal(buf[24:36], m.nonce[:]) || buf[36] != udpProto || binary.BigEndian.Uint16(buf[40:42]) != m.port {
			return false, nil
		}
		if buf[3] != 0 {
			return true, routerResult{"PCP", uint16(buf[3])}
		}
		lifetime = binary.BigEndian.Uint32(buf[4:8])
		port := binary.BigEndian.Uint16(buf[42:44])
		ip := netip.AddrFrom16([16]byte(buf[44:60])).Unmap()
		if seconds != 0 && lifetime != 0 {
			if port == 0 || ip.IsUnspecified() {
				return false, nil
			}
			endpoint = netip.AddrPortFrom(ip, port)
		} else if port != 0 || !ip.IsUnspecified() {
			return false, nil
		}
		return true, nil
	})
	return endpoint, lifetime, err
}

func (m *pcpMapping) refresh(ctx context.Context, old lease) (lease, error) {
	started := time.Now()
	endpoint, seconds, err := m.transact(ctx, requestedSeconds, old.endpoint)
	if err != nil {
		return uncertainWrite(started, err), err
	}
	if seconds == 0 {
		return lease{}, errLeaseLost
	}
	// RFC 6887 specifies all-ones as the suggested lifetime for an existing
	// static mapping. Report it, but never claim ownership of it.
	owned := seconds != ^uint32(0)
	return acknowledgedLease(endpoint, min(seconds, 86400), started, owned), nil
}

func (m *pcpMapping) remove(ctx context.Context, _ lease) error {
	_, seconds, err := m.transact(ctx, 0, netip.AddrPort{})
	if err == nil && seconds != 0 {
		// RFC 6887 may retain an idle dynamic mapping after deleting its
		// explicit lease. Let it time out rather than switching protocol.
		return errors.New("PCP retained a dynamic mapping")
	}
	return err
}

type pmpWAN struct {
	ip      netip.Addr
	epoch   uint32
	started time.Time
}

type pmpMapping struct {
	router  router
	port    uint16
	initial pmpWAN
}

func (r router) getPMPWAN(ctx context.Context) (pmpWAN, error) {
	wan := pmpWAN{started: time.Now()}
	err := r.udpRequest(ctx, []byte{0, 0}, true, func(buf []byte) (bool, error) {
		if len(buf) != 8 && len(buf) != 12 || buf[0] != 0 || buf[1] != 0x80 {
			return false, nil
		}
		if result := binary.BigEndian.Uint16(buf[2:4]); result != 0 {
			return true, routerResult{"NAT-PMP", result}
		}
		if len(buf) != 12 {
			return false, nil
		}
		wan.epoch = binary.BigEndian.Uint32(buf[4:8])
		wan.ip = netip.AddrFrom4([4]byte(buf[8:12]))
		return true, nil
	})
	return wan, err
}

func (r router) probePMP(ctx context.Context, port uint16) (mapping, error) {
	wan, err := r.getPMPWAN(ctx)
	if err != nil {
		return nil, err
	}
	if !usableEndpoint(netip.AddrPortFrom(wan.ip, port)) {
		return nil, errors.New("NAT-PMP did not report a usable external IPv4 address")
	}
	return &pmpMapping{router: r, port: port, initial: wan}, nil
}

func (m *pmpMapping) transact(ctx context.Context, wantPort uint16, wantSeconds uint32) (port uint16, seconds, epoch uint32, err error) {
	var req [12]byte
	req[1] = 1 // UDP mapping; TCP has its own opcode.
	binary.BigEndian.PutUint16(req[4:6], m.port)
	binary.BigEndian.PutUint16(req[6:8], wantPort)
	binary.BigEndian.PutUint32(req[8:12], wantSeconds)
	err = m.router.udpRequest(ctx, req[:], false, func(buf []byte) (bool, error) {
		if len(buf) != 16 || buf[0] != 0 || buf[1] != 0x81 || binary.BigEndian.Uint16(buf[8:10]) != m.port {
			return false, nil
		}
		if result := binary.BigEndian.Uint16(buf[2:4]); result != 0 {
			return true, routerResult{"NAT-PMP", result}
		}
		port, seconds = binary.BigEndian.Uint16(buf[10:12]), binary.BigEndian.Uint32(buf[12:16])
		if wantSeconds == 0 && (seconds != 0 || port != 0) || wantSeconds != 0 && (seconds == 0 || port == 0) {
			return false, nil
		}
		epoch = binary.BigEndian.Uint32(buf[4:8])
		return true, nil
	})
	return
}

func samePMPEpoch(old, next uint32, since time.Time) bool {
	// RFC 6886 §3.6: trust only forward progress allowing for clock skew.
	expected := int64(old) + int64(time.Since(since)*7/(8*time.Second))
	return int64(next)+2 >= expected
}

func (m *pmpMapping) refresh(ctx context.Context, old lease) (lease, error) {
	wan, suggested := m.initial, m.port
	if old.endpoint.IsValid() {
		var err error
		wan, err = m.router.getPMPWAN(ctx)
		if err != nil {
			return lease{}, err
		}
		if !usableEndpoint(netip.AddrPortFrom(wan.ip, m.port)) {
			return lease{}, errLeaseLost
		}
		suggested = old.endpoint.Port()
	}
	reset := !old.started.IsZero() && !samePMPEpoch(old.epoch, wan.epoch, old.started)
	started := time.Now()
	port, seconds, epoch, err := m.transact(ctx, suggested, requestedSeconds)
	if err != nil {
		if reset {
			err = errors.Join(err, errLeaseLost)
		}
		return uncertainWrite(started, err), err
	}
	current := acknowledgedLease(netip.AddrPortFrom(wan.ip, port), min(seconds, 86400), started, true)
	current.epoch = epoch
	if !samePMPEpoch(wan.epoch, epoch, wan.started) {
		// The first WAN address is no longer reliable. Re-query after mapping;
		// if this fails, retain the confirmed lease for scoped cleanup only.
		newWAN, err := m.router.getPMPWAN(ctx)
		if err != nil || !samePMPEpoch(epoch, newWAN.epoch, started) {
			current.endpoint = netip.AddrPortFrom(netip.IPv4Unspecified(), port)
			return current, errLeaseLost
		}
		current.endpoint = netip.AddrPortFrom(newWAN.ip, port)
	}
	return current, nil
}

func (m *pmpMapping) remove(ctx context.Context, current lease) error {
	// NAT-PMP has no nonce or way to query ownership by port. A finite
	// acknowledgement for the owned UDP listener and an unchanged router
	// epoch are the best identification this protocol provides. Compliant
	// routers independently reject deletion of manually assigned mappings.
	wan, err := m.router.getPMPWAN(ctx)
	if err != nil {
		return err
	}
	if !samePMPEpoch(current.epoch, wan.epoch, current.started) {
		return errLeaseLost
	}
	_, _, _, err = m.transact(ctx, 0, 0)
	return err
}
