package direct

import (
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/tunsetup"

	"github.com/anacrolix/go-utp/purego"
)

const streamAttemptTimeout = 900 * time.Millisecond

type connectResult struct {
	transport carrier.Transport
	err       error
}

// Connect preserves the legacy carrier-only API.
func (s *Socket) Connect(ctx context.Context, offer Offer) (carrier.Transport, error) {
	tr, _, _, err := s.ConnectStream(ctx, offer)
	return tr, err
}

// ConnectStream returns the carrier, its underlying connection, and whether this
// endpoint dialed the stream. The returned connection is owned by the carrier.
func (s *Socket) ConnectStream(ctx context.Context, offer Offer) (carrier.Transport, net.Conn, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if !time.Now().Before(offer.Expires) {
		return nil, nil, false, context.DeadlineExceeded
	}
	if offer.Secret == ([32]byte{}) {
		return nil, nil, false, errors.New("empty direct offer secret")
	}
	mode := offer.Mode
	if mode == "" {
		mode = config.ModeExtUTP
	}
	if mode != config.CanonicalMode(mode) || !config.IsCarrierMode(mode) {
		return nil, nil, false, errors.New("invalid direct carrier mode")
	}
	tcp := mode == config.ModeExtTCP || mode == config.ModeReqTCP || mode == config.ModeTLSExtTCP || mode == config.ModeTLSReqTCP
	ctx, cancel := context.WithDeadline(ctx, offer.Expires)
	stop := context.AfterFunc(s.ctx, cancel)
	defer cancel()
	defer stop()
	var modeBytes []byte
	if offer.Mode != "" {
		modeBytes = []byte(mode)
	}
	key := mac(offer.Secret, "btawg/direct/pair/1", modeBytes)
	id := mac(key, "btawg/direct/selector/1", nil)
	a := &attempt{
		ctx:       ctx,
		key:       key,
		id:        token(id[:16]),
		initiator: offer.Initiator,
		framing:   config.CarrierFraming(mode),
		tcp:       tcp,
		paths:     make(map[netip.AddrPort]*path),
		wake:      make(chan struct{}, 1),
		winner:    make(chan connectResult),
	}
	for _, candidate := range offer.Candidates {
		addr := normalize(candidate.Addr)
		if len(a.order) >= maxCandidates {
			break
		}
		if !usable(addr) {
			continue
		}
		if tcp {
			if candidate.Protocol != "tcp" {
				continue
			}
			if len(a.tcpCandidates) >= maxCandidates {
				break
			}
			a.tcpCandidates = append(a.tcpCandidates, addr)
		} else if (candidate.Protocol == "" || candidate.Protocol == "udp") && s.familyFor(addr.Addr()) != nil && a.paths[addr] == nil {
			s.addPath(a, addr, s.familyFor(addr.Addr())).listed = true
		}
	}
	if tcp {
		for _, candidate := range s.LocalCandidates() {
			if candidate.Protocol == "" || candidate.Protocol == "udp" {
				a.tcpLocal = append(a.tcpLocal, candidate.Addr.Addr())
			}
		}
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, nil, false, net.ErrClosed
	case s.attempts[a.id] != nil:
		s.mu.Unlock()
		return nil, nil, false, errors.New("direct offer already in flight")
	}
	s.workers.Add(1)
	s.attempts[a.id] = a
	s.mu.Unlock()
	var punchDone chan struct{}
	if !tcp {
		punchDone = make(chan struct{})
		go s.punch(a, punchDone)
	}
	defer func() {
		s.mu.Lock()
		delete(s.attempts, a.id)
		s.mu.Unlock()
		cancel()
		if punchDone != nil {
			<-punchDone
		}
		s.workers.Done()
	}()

	var tr carrier.Transport
	var err error
	if tcp {
		tr, err = s.connectTCP(a)
	} else if a.initiator {
		tr, err = s.dial(a)
	} else {
		select {
		case result := <-a.winner:
			tr, err = result.transport, result.err
		case <-ctx.Done():
		}
	}
	if s.ctx.Err() != nil {
		err = net.ErrClosed
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil && tr != nil {
		tr.Wait()
		tr = nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return tr, a.conn, a.dialer, nil
}

func (s *Socket) dial(a *attempt) (carrier.Transport, error) {
	var next int
	retry := time.NewTicker(punchInterval)
	defer retry.Stop()
	for a.ctx.Err() == nil {
		var p *path
		s.mu.Lock()
		for range len(a.order) {
			candidate := a.order[next%len(a.order)]
			next = (next + 1) % len(a.order)
			if candidate.dialable {
				p = candidate
				break
			}
		}
		s.mu.Unlock()
		if p != nil {
			tr, committed, err := s.dialPath(a, p)
			if committed {
				return tr, err
			}
		}
		select {
		case <-a.ctx.Done():
		case <-a.wake:
		case <-retry.C:
		}
	}
	return nil, a.ctx.Err()
}

func (s *Socket) dialPath(a *attempt, p *path) (carrier.Transport, bool, error) {
	dialCtx, cancel := context.WithTimeout(a.ctx, streamAttemptTimeout)
	defer cancel()
	c, err := p.family.DialAddrContext(dialCtx, p.remote)
	if err != nil {
		return nil, false, err
	}
	stop := context.AfterFunc(a.ctx, func() { _ = c.Close() })
	var tr carrier.Transport
	defer func() {
		stop()
		if tr == nil {
			_ = c.Close()
		}
	}()
	deadline, _ := dialCtx.Deadline()
	_ = c.SetDeadline(deadline)
	var hello [32]byte
	copy(hello[:16], a.id[:])
	random(hello[16:])
	if err := writeMessage(c, hello[:]); err != nil {
		return nil, false, err
	}
	var reply [48]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return nil, false, err
	}
	var transcript [32]byte
	copy(transcript[:16], hello[16:])
	copy(transcript[16:], reply[:16])
	want := mac(a.key, "btawg/direct/stream/responder/1", transcript[:])
	if !hmac.Equal(reply[16:], want[:]) {
		return nil, false, errors.New("invalid direct responder proof")
	}
	if err := a.ctx.Err(); err != nil {
		return nil, false, err
	}
	// Sending the authenticated initiator proof nominates this stream. Do
	// not try a different stream after it could have committed at the peer.
	if !s.commit(a) {
		return nil, false, errors.New("direct offer already committed")
	}
	deadline, _ = a.ctx.Deadline()
	_ = c.SetDeadline(deadline)
	proof := mac(a.key, "btawg/direct/stream/initiator/1", transcript[:])
	if err := writeMessage(c, proof[:]); err != nil {
		return nil, true, err
	}
	streamKey := mac(a.key, "btawg/direct/stream/carrier/1", transcript[:])
	tr, err = s.streamCarrier(a.ctx, c, streamKey, a.framing, true)
	if err == nil {
		a.conn, a.dialer = c, true
	}
	if err == nil && !stop() {
		tr.Wait()
		tr, err = nil, a.ctx.Err()
	}
	return tr, true, err
}

func (s *Socket) accept(f *purego.Socket) {
	for {
		c, err := f.Accept()
		if err != nil {
			return
		}
		select {
		case s.accepting <- struct{}{}:
			s.workers.Go(func() {
				defer func() { <-s.accepting }()
				s.inbound(c)
			})
		default:
			_ = c.Close()
		}
	}
}

func (s *Socket) inbound(c net.Conn) {
	var tr carrier.Transport
	defer func() {
		if tr == nil {
			_ = c.Close()
		}
	}()
	deadline := time.Now().Add(streamAttemptTimeout)
	_ = c.SetDeadline(deadline)
	var hello [32]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil {
		return
	}
	id := token(hello[:16])
	source := remoteAddr(c.RemoteAddr())
	s.mu.Lock()
	a := s.attempts[id]
	allowed := !s.closed && a != nil && !a.tcp && !a.initiator && !a.committed && a.ctx.Err() == nil
	if allowed {
		p := a.paths[source]
		allowed = p != nil && p.proved
	}
	s.mu.Unlock()
	if !allowed {
		return
	}
	stop := context.AfterFunc(a.ctx, func() { _ = c.Close() })
	defer stop()
	lease, _ := a.ctx.Deadline()
	if lease.Before(deadline) {
		_ = c.SetDeadline(lease)
	}
	var transcript [32]byte
	copy(transcript[:16], hello[16:])
	random(transcript[16:])
	var reply [48]byte
	copy(reply[:16], transcript[16:])
	proof := mac(a.key, "btawg/direct/stream/responder/1", transcript[:])
	copy(reply[16:], proof[:])
	if err := writeMessage(c, reply[:]); err != nil {
		return
	}
	var got [32]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		return
	}
	want := mac(a.key, "btawg/direct/stream/initiator/1", transcript[:])
	if !hmac.Equal(got[:], want[:]) {
		return
	}
	s.mu.Lock()
	if s.closed || s.attempts[id] != a || a.committed || a.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	a.committed = true
	s.mu.Unlock()
	_ = c.SetDeadline(lease)
	streamKey := mac(a.key, "btawg/direct/stream/carrier/1", transcript[:])
	var err error
	tr, err = s.streamCarrier(a.ctx, c, streamKey, a.framing, false)
	if err == nil {
		a.conn, a.dialer = c, false
	}
	if err != nil {
		deliver(a, connectResult{err: err})
		return
	}
	// Disconnect the negotiation cancellation before handing ownership to
	// Connect, which always cancels its private context before returning.
	if !stop() {
		tr.Wait()
		return
	}
	deliver(a, connectResult{transport: tr})
}

func deliver(a *attempt, result connectResult) {
	select {
	case a.winner <- result:
	case <-a.ctx.Done():
		if result.transport != nil {
			result.transport.Wait()
		}
	}
}

func writeMessage(c net.Conn, b []byte) error {
	for len(b) != 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func (s *Socket) commit(a *attempt) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.attempts[a.id] != a || a.committed || a.ctx.Err() != nil {
		return false
	}
	a.committed = true
	return true
}

func (s *Socket) acceptTCP() {
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			return
		}
		remote, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			_ = c.Close()
			continue
		}
		c, err = tunsetup.ProtectUnderlayStream(c, remote.AddrPort().Addr())
		if err != nil {
			continue
		}
		select {
		case s.accepting <- struct{}{}:
			s.workers.Go(func() { defer func() { <-s.accepting }(); s.inboundTCP(c) })
		default:
			_ = c.Close()
		}
	}
}

func (s *Socket) connectTCP(a *attempt) (carrier.Transport, error) {
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.dialTCP(ctx, a) }()
	defer func() { cancel(); <-done }()
	select {
	case result := <-a.winner:
		return result.transport, result.err
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	}
}

func (s *Socket) dialTCP(ctx context.Context, a *attempt) {
	if !a.initiator {
		timer := time.NewTimer(streamAttemptTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}
	ticker := time.NewTicker(punchInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		for _, addr := range a.tcpCandidates {
			attemptCtx, cancel := context.WithTimeout(ctx, streamAttemptTimeout)
			var guard tunsetup.UnderlayDialGuard
			dialer := net.Dialer{Control: guard.Control}
			c, err := dialer.DialContext(attemptCtx, "tcp", addr.String())
			if err == nil {
				c, err = guard.Protect(c, addr.Addr())
			}
			if err == nil {
				err = s.outboundTCP(attemptCtx, a, c)
			}
			cancel()
			if err == nil || ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Socket) outboundTCP(ctx context.Context, a *attempt, c net.Conn) error {
	var tr carrier.Transport
	defer func() {
		if tr == nil {
			_ = c.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	var infoHash, peerID [20]byte
	copy(infoHash[:16], a.id[:])
	random(peerID[:16])
	if a.initiator {
		peerID[16] = 1
	}
	hello := carrier.HandshakeBytes(infoHash, peerID)
	tag := mac(a.key, "btawg/direct/tcp/hello/1", hello)
	if err := writeMessage(c, hello); err != nil {
		return err
	}
	if err := writeMessage(c, tag[:]); err != nil {
		return err
	}
	var reply [48]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return err
	}
	var transcript [33]byte
	copy(transcript[:16], peerID[:16])
	copy(transcript[16:32], reply[:16])
	transcript[32] = peerID[16]
	want := mac(a.key, "btawg/direct/tcp/responder/1", transcript[:])
	if !hmac.Equal(want[:], reply[16:]) {
		return errors.New("invalid TCP responder proof")
	}
	if a.initiator && !s.commit(a) {
		return errors.New("direct offer already committed")
	}
	proof := mac(a.key, "btawg/direct/tcp/initiator/1", transcript[:])
	if err := writeMessage(c, proof[:]); err != nil {
		return err
	}
	var ack [32]byte
	if _, err := io.ReadFull(c, ack[:]); err != nil {
		return err
	}
	want = mac(a.key, "btawg/direct/tcp/commit/1", transcript[:])
	if !hmac.Equal(ack[:], want[:]) {
		return errors.New("invalid TCP commit proof")
	}
	if !a.initiator && !s.commit(a) {
		return errors.New("direct offer already committed")
	}
	lease, _ := a.ctx.Deadline()
	_ = c.SetDeadline(lease)
	key := mac(a.key, "btawg/direct/tcp/carrier/1", transcript[:])
	var err error
	tr, err = s.streamCarrier(a.ctx, c, key, a.framing, true)
	if err != nil {
		deliver(a, connectResult{err: err})
		return nil
	}
	if !stop() {
		tr.Wait()
		tr = nil
		return ctx.Err()
	}
	a.conn, a.dialer = c, true
	deliver(a, connectResult{transport: tr})
	return nil
}

func (s *Socket) inboundTCP(c net.Conn) {
	var tr carrier.Transport
	defer func() {
		if tr == nil {
			_ = c.Close()
		}
	}()
	_ = c.SetDeadline(time.Now().Add(streamAttemptTimeout))
	var hello [68]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil {
		return
	}
	if hello[0] != 19 || string(hello[1:20]) != "BitTorrent protocol" {
		return
	}
	id := token(hello[28:44])
	if hello[64] > 1 {
		return
	}
	local, ok := c.LocalAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	localIP := local.AddrPort().Addr().Unmap()
	s.mu.Lock()
	a := s.attempts[id]
	allowed := !s.closed && a != nil && a.tcp && !a.committed && a.ctx.Err() == nil && a.initiator != (hello[64] == 1)
	if allowed && !localIP.IsLoopback() {
		allowed = false
		for _, host := range a.tcpLocal {
			if host == localIP {
				allowed = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !allowed {
		return
	}
	stop := context.AfterFunc(a.ctx, func() { _ = c.Close() })
	defer stop()
	var tag [32]byte
	if _, err := io.ReadFull(c, tag[:]); err != nil {
		return
	}
	want := mac(a.key, "btawg/direct/tcp/hello/1", hello[:])
	if !hmac.Equal(tag[:], want[:]) {
		return
	}
	var transcript [33]byte
	copy(transcript[:16], hello[48:64])
	random(transcript[16:32])
	transcript[32] = hello[64]
	var reply [48]byte
	copy(reply[:16], transcript[16:32])
	proof := mac(a.key, "btawg/direct/tcp/responder/1", transcript[:])
	copy(reply[16:], proof[:])
	if err := writeMessage(c, reply[:]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, tag[:]); err != nil {
		return
	}
	want = mac(a.key, "btawg/direct/tcp/initiator/1", transcript[:])
	if !hmac.Equal(tag[:], want[:]) {
		return
	}
	if !s.commit(a) {
		return
	}
	ack := mac(a.key, "btawg/direct/tcp/commit/1", transcript[:])
	if err := writeMessage(c, ack[:]); err != nil {
		return
	}
	lease, _ := a.ctx.Deadline()
	_ = c.SetDeadline(lease)
	key := mac(a.key, "btawg/direct/tcp/carrier/1", transcript[:])
	var err error
	tr, err = s.streamCarrier(a.ctx, c, key, a.framing, false)
	if err != nil {
		deliver(a, connectResult{err: err})
		return
	}
	if !stop() {
		tr.Wait()
		tr = nil
		return
	}
	a.conn, a.dialer = c, false
	deliver(a, connectResult{transport: tr})
}
