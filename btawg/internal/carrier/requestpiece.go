package carrier

import (
	"bufio"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Each endpoint advertises one half of the virtual index space. Indices cycle
// after the outstanding requests have drained past them.
const (
	legacyRequestWindow      = 32
	wideRequestFeature       = 1
	compactRequestFeature    = 2
	windowRequestFeature     = 4
	variedRequestFeature     = 8
	autoVariedRequestFeature = 16 // optional; old peers ignore it
	compactBlockSize         = 1536
	pieceContinuation        = 1 << 15
	// One credit carries one datagram. 12,288 credits leave room for 500
	// Mbit/s of full-sized TCP payloads at 200 ms RTT.
	requestWindow = 12288
	totalPieces   = 65536
)

// Wire-format experiment, NOT a valid immutable-file torrent: AWG ciphertext
// replaces file blocks, so file hashes never validate.
type request uint32

type requestPieceCarrier struct {
	c            net.Conn
	writeMu      sync.Mutex
	sendMu       sync.Mutex
	stateMu      sync.Mutex
	rndMu        sync.Mutex
	pieceFrame   [13 + BlockSize]byte
	pieceBatch   []byte
	requests     chan request
	replacements chan [17]byte
	received     chan []byte
	done         chan struct{}
	once         sync.Once
	wg           sync.WaitGroup
	pending      map[uint32]bool
	outBegin     map[uint32]uint32
	outLen       map[uint32]int
	inBegin      map[uint32]uint32
	inLen        map[uint32]int
	next         uint32
	window       int // negotiated maximum in-flight requests
	curWindow    int // current adaptive in-flight limit
	fastCount    int // full-window consumption events in the current epoch
	blockSize    int
	experiment   byte
	ownOdd       bool
	invalid      atomic.Int64
	rnd          *rand.Rand
}

// RequestFeatures reads the BTAWG_REQ_EXPERIMENT environment variable and
// returns the feature byte this process advertises for request/piece carriers.
// The baseline always includes wide blocks and compact blocks; window and
// varied are added when the corresponding experiment is enabled. Varied
// implies window.
func RequestFeatures() (byte, error) {
	features := byte(wideRequestFeature | compactRequestFeature)
	switch os.Getenv("BTAWG_REQ_EXPERIMENT") {
	case "":
		features |= autoVariedRequestFeature
	case "baseline":
	case "window":
		features |= windowRequestFeature
	case "varied":
		features |= windowRequestFeature | variedRequestFeature
	default:
		return 0, fmt.Errorf("invalid BTAWG_REQ_EXPERIMENT value %q (want baseline, window, or varied)", os.Getenv("BTAWG_REQ_EXPERIMENT"))
	}
	return features, nil
}

func validateRequestFeatures(local, remote byte) error {
	exp := byte(windowRequestFeature | variedRequestFeature)
	if local&exp != remote&exp {
		return fmt.Errorf("request/piece experiment mismatch: local=%02x remote=%02x", local, remote)
	}
	return nil
}

// NewRequestPiece wraps an established connection with BEP 3 request/piece
// framing for direct peers. It advertises only the baseline wide|compact
// features and rejects peers that require the window/varied experiments.
func NewRequestPiece(c net.Conn, infoHash, peerID [20]byte, ownOdd bool) (Transport, [20]byte, error) {
	local := byte(wideRequestFeature | compactRequestFeature)
	theirs, peerFeatures, err := handshake(c, infoHash, peerID, local)
	if err != nil {
		return nil, theirs, err
	}
	if err := validateRequestFeatures(local, peerFeatures); err != nil {
		c.Close()
		return nil, theirs, err
	}
	x := newRequestPieceCarrier(c, local, peerFeatures)
	x.start(ownOdd)
	return x, theirs, nil
}

// NewHubRequestPiece is the hub/client entrypoint for the request/piece
// carrier. It reads BTAWG_REQ_EXPERIMENT, advertises the configured feature
// bits, and fails closed when the peer does not match.
func NewHubRequestPiece(c net.Conn, infoHash, peerID [20]byte, ownOdd bool) (Transport, [20]byte, error) {
	var zero [20]byte
	local, err := RequestFeatures()
	if err != nil {
		return nil, zero, err
	}
	theirs, peerFeatures, err := handshake(c, infoHash, peerID, local)
	if err != nil {
		return nil, theirs, err
	}
	if err := validateRequestFeatures(local, peerFeatures); err != nil {
		c.Close()
		return nil, theirs, err
	}
	x := newRequestPieceCarrier(c, local, peerFeatures)
	x.start(ownOdd)
	return x, theirs, nil
}

// acceptRequestPiece finishes a server-side direct request/piece carrier after
// the initial BitTorrent handshake has already been consumed by Accept. It
// uses the baseline feature set and rejects experimental peers.
func acceptRequestPiece(c net.Conn, peerFeatures byte) (Transport, error) {
	local := byte(wideRequestFeature | compactRequestFeature)
	if err := validateRequestFeatures(local, peerFeatures); err != nil {
		c.Close()
		return nil, err
	}
	x := newRequestPieceCarrier(c, local, peerFeatures)
	x.start(true)
	return x, nil
}

// acceptHubRequestPiece finishes a server-side hub request/piece carrier. The
// caller obtains local features from RequestFeatures and has already written
// them into the handshake.
func acceptHubRequestPiece(c net.Conn, local, remote byte) (Transport, error) {
	if err := validateRequestFeatures(local, remote); err != nil {
		c.Close()
		return nil, err
	}
	x := newRequestPieceCarrier(c, local, remote)
	x.start(true)
	return x, nil
}

func newRequestPieceCarrier(c net.Conn, local, remote byte) *requestPieceCarrier {
	blockSize := BlockSize
	if remote&compactRequestFeature != 0 {
		blockSize = compactBlockSize
	}
	maxWindow := legacyRequestWindow
	if remote&wideRequestFeature != 0 {
		maxWindow = requestWindow
	}
	experiment := local & (windowRequestFeature | variedRequestFeature)
	if local&remote&autoVariedRequestFeature != 0 {
		experiment = windowRequestFeature | variedRequestFeature
	}
	curWindow := maxWindow
	if experiment != 0 {
		curWindow = legacyRequestWindow
	}
	return &requestPieceCarrier{
		c:            c,
		requests:     make(chan request, maxWindow),
		replacements: make(chan [17]byte, maxWindow),
		received:     make(chan []byte, receiveQueuePackets),
		done:         make(chan struct{}),
		pending:      make(map[uint32]bool, maxWindow),
		outBegin:     make(map[uint32]uint32),
		outLen:       make(map[uint32]int),
		inBegin:      make(map[uint32]uint32),
		inLen:        make(map[uint32]int),
		window:       maxWindow,
		curWindow:    curWindow,
		blockSize:    blockSize,
		experiment:   experiment,
		rnd:          rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)),
	}
}

func (x *requestPieceCarrier) start(ownOdd bool) {
	x.ownOdd = ownOdd
	own := make([]byte, totalPieces/8)
	for i := range totalPieces {
		if (i%2 == 1) == ownOdd {
			own[i/8] |= 1 << uint(7-i%8)
		}
	}
	x.next = 0
	if !ownOdd {
		x.next = 1
	}
	x.wg.Add(1)
	go func() {
		defer x.wg.Done()
		x.readLoop()
	}()
	x.wg.Add(1)
	go x.writeReplacements()
	if err := x.message(5, own); err != nil {
		x.Close()
		return
	}
	if err := x.message(2, nil); err != nil {
		x.Close()
		return
	}
	if err := x.message(1, nil); err != nil {
		x.Close()
		return
	}
}

func (x *requestPieceCarrier) message(id byte, p []byte) error {
	b := make([]byte, 5+len(p))
	binary.BigEndian.PutUint32(b, uint32(len(p)+1))
	b[4] = id
	copy(b[5:], p)
	x.writeMu.Lock()
	defer x.writeMu.Unlock()
	return fullWrite(x.c, b)
}

func (x *requestPieceCarrier) nextRequest() [17]byte {
	x.stateMu.Lock()
	defer x.stateMu.Unlock()
	idx := x.nextIndex()
	begin, length := uint32(0), x.wireBlockSize()
	if x.experiment&variedRequestFeature != 0 {
		begin, length = x.randomBlock()
		x.outBegin[idx] = begin
		x.outLen[idx] = length
	}
	x.pending[idx] = true
	var b [17]byte
	binary.BigEndian.PutUint32(b[:], 13)
	b[4] = 6
	binary.BigEndian.PutUint32(b[5:], idx)
	binary.BigEndian.PutUint32(b[9:], begin)
	binary.BigEndian.PutUint32(b[13:], uint32(length))
	return b
}

func (x *requestPieceCarrier) nextIndex() uint32 {
	if x.experiment&variedRequestFeature != 0 {
		half := totalPieces / 2
		x.rndMu.Lock()
		start := uint32(x.rnd.IntN(half)) * 2
		if !x.ownOdd {
			start++
		}
		x.rndMu.Unlock()
		idx := start
		for x.pending[idx] {
			idx = (idx + 2) % totalPieces
			if idx == start {
				break
			}
		}
		return idx
	}
	idx := x.next
	for x.pending[idx] {
		idx = (idx + 2) % totalPieces
	}
	x.next = (idx + 2) % totalPieces
	return idx
}

func (x *requestPieceCarrier) randomBlock() (uint32, int) {
	x.rndMu.Lock()
	defer x.rndMu.Unlock()
	maxBlock := x.maxBlockSize()
	minBlock := compactBlockSize
	if minBlock > maxBlock {
		minBlock = maxBlock
	}
	length := minBlock + x.rnd.IntN(maxBlock-minBlock+1)
	maxBegin := maxBlock - length
	begin := 0
	if maxBegin > 0 {
		begin = x.rnd.IntN(maxBegin + 1)
		if begin == 0 && x.rnd.IntN(4) != 0 {
			begin = x.rnd.IntN(maxBegin) + 1
		}
	}
	return uint32(begin), length
}

func (x *requestPieceCarrier) issueCredit() bool {
	b := x.nextRequest()
	select {
	case x.replacements <- b:
		return true
	case <-x.done:
		return false
	}
}

func (x *requestPieceCarrier) queueReplacement() bool {
	return x.issueCredit()
}

func (x *requestPieceCarrier) replenish(full bool) bool {
	x.stateMu.Lock()
	if x.experiment&windowRequestFeature != 0 && x.curWindow < x.window {
		if full {
			x.fastCount++
			if x.fastCount >= x.curWindow {
				newWindow := x.curWindow * 2
				if newWindow > x.window {
					newWindow = x.window
				}
				x.curWindow = newWindow
				x.fastCount = 0
				x.stateMu.Unlock()
				return x.topUp()
			}
		} else {
			x.fastCount = 0
		}
	}
	x.stateMu.Unlock()
	return x.issueCredit()
}

func (x *requestPieceCarrier) topUp() bool {
	for {
		x.stateMu.Lock()
		needed := x.curWindow - len(x.pending)
		x.stateMu.Unlock()
		if needed <= 0 {
			return true
		}
		if !x.issueCredit() {
			return false
		}
	}
}

func (x *requestPieceCarrier) resetEpoch() {
	x.stateMu.Lock()
	x.fastCount = 0
	x.stateMu.Unlock()
}

func (x *requestPieceCarrier) writeReplacements() {
	defer x.wg.Done()
	// Drain credits already waiting without a timer. More room matters
	// when the writer has been blocked behind a data batch.
	const batch = 256
	var requests [17 * batch]byte
	for {
		select {
		case <-x.done:
			return
		case b := <-x.replacements:
			copy(requests[:], b[:])
		}
		n := 17
		x.writeMu.Lock()
		for n < len(requests) && len(x.replacements) != 0 {
			b := <-x.replacements
			copy(requests[n:], b[:])
			n += len(b)
		}
		err := fullWrite(x.c, requests[:n])
		x.writeMu.Unlock()
		if err != nil {
			x.Close()
			return
		}
	}
}

func (x *requestPieceCarrier) readLoop() {
	defer x.Close()
	reader := bufio.NewReaderSize(x.c, streamReadBuffer)
	blockSize := x.wireBlockSize()
	var partial []byte
	bitfield, unchoked, primed := false, false, false
	for {
		h, err := reader.Peek(4)
		if err != nil {
			return
		}
		length := binary.BigEndian.Uint32(h)
		reader.Discard(4)
		if length == 0 {
			continue
		}
		if length > 1+totalPieces/8 {
			x.invalid.Add(1)
			return
		}
		n := int(length)
		b, err := reader.Peek(n)
		if err != nil {
			return
		}
		switch b[0] {
		case 5:
			if len(b) != 1+totalPieces/8 {
				x.invalid.Add(1)
				return
			}
			bitfield = true
		case 1:
			unchoked = true
		case 2:
			// interested: no state needed
		case 6:
			if len(b) != 13 {
				x.invalid.Add(1)
				return
			}
			idx := binary.BigEndian.Uint32(b[1:])
			off := binary.BigEndian.Uint32(b[5:])
			reqLen := binary.BigEndian.Uint32(b[9:])
			if idx >= totalPieces || idx%2 == x.next%2 {
				x.invalid.Add(1)
				return
			}
			if x.experiment&variedRequestFeature != 0 {
				// reqLen == 2 leaves zero payload bytes, making the request
				// unanswerable; off+reqLen must not wrap past maxBlockSize.
				if reqLen < 3 || reqLen > uint32(x.maxBlockSize()) || off > uint32(x.maxBlockSize())-reqLen {
					x.invalid.Add(1)
					return
				}
				x.stateMu.Lock()
				if _, exists := x.inBegin[idx]; exists {
					x.stateMu.Unlock()
					x.invalid.Add(1)
					return
				}
				x.inBegin[idx] = off
				x.inLen[idx] = int(reqLen)
				x.stateMu.Unlock()
			} else {
				if off != 0 || reqLen != uint32(blockSize) {
					x.invalid.Add(1)
					return
				}
			}
			select {
			case x.requests <- request(idx):
			case <-x.done:
				return
			default:
				x.invalid.Add(1)
				return
			}
		case 7:
			if len(b) < 11 {
				x.invalid.Add(1)
				return
			}
			idx := binary.BigEndian.Uint32(b[1:])
			off := binary.BigEndian.Uint32(b[5:])
			lengthField := binary.BigEndian.Uint16(b[9:])
			continued := lengthField&pieceContinuation != 0
			size := int(lengthField &^ pieceContinuation)
			blockLen := len(b) - 9
			x.stateMu.Lock()
			if !x.pending[idx] {
				x.stateMu.Unlock()
				x.invalid.Add(1)
				return
			}
			wantBegin := uint32(0)
			wantLen := blockSize
			if x.experiment&variedRequestFeature != 0 {
				beginOK := false
				lenOK := false
				wantBegin, beginOK = x.outBegin[idx]
				wantLen, lenOK = x.outLen[idx]
				if !beginOK || !lenOK {
					x.stateMu.Unlock()
					x.invalid.Add(1)
					return
				}
			}
			if off != wantBegin || blockLen != wantLen {
				x.stateMu.Unlock()
				x.invalid.Add(1)
				return
			}
			full := len(x.pending) == x.curWindow
			delete(x.pending, idx)
			if x.experiment&variedRequestFeature != 0 {
				delete(x.outBegin, idx)
				delete(x.outLen, idx)
			}
			x.stateMu.Unlock()
			if size == 0 || size > wantLen-2 || (continued && size != wantLen-2) || len(partial)+size > BlockSize-2 {
				x.invalid.Add(1)
				return
			}
			p := b[11 : 11+size]
			if continued {
				partial = make([]byte, size, BlockSize-2)
				copy(partial, p)
			} else {
				p = append(partial, p...)
				partial = nil
				select {
				case x.received <- p:
				case <-x.done:
					return
				}
			}
			if full {
				if !x.replenish(true) {
					return
				}
			} else {
				x.resetEpoch()
				if !x.issueCredit() {
					return
				}
			}
		default:
			x.invalid.Add(1)
			return
		}
		reader.Discard(n)
		if bitfield && unchoked && !primed {
			primed = true
			for range x.curWindow {
				if !x.issueCredit() {
					return
				}
			}
		}
	}
}

func (x *requestPieceCarrier) wireBlockSize() int {
	if x.blockSize == 0 {
		return BlockSize
	}
	return x.blockSize
}

func (x *requestPieceCarrier) maxBlockSize() int {
	if x.experiment&variedRequestFeature != 0 {
		return BlockSize
	}
	return x.wireBlockSize()
}

func (x *requestPieceCarrier) Send(p []byte) error {
	if len(p) == 0 || len(p) > BlockSize-2 {
		return fmt.Errorf("invalid AWG datagram size: %d", len(p))
	}
	x.sendMu.Lock()
	defer x.sendMu.Unlock()
	return x.sendDatagram(p)
}

func (x *requestPieceCarrier) sendDatagram(p []byte) error {
	for len(p) > 0 {
		var r request
		select {
		case r = <-x.requests:
		case <-x.done:
			return net.ErrClosed
		}
		idx := uint32(r)
		begin := uint32(0)
		maxPayload := x.wireBlockSize() - 2
		if x.experiment&variedRequestFeature != 0 {
			x.stateMu.Lock()
			begin = x.inBegin[idx]
			if l, ok := x.inLen[idx]; ok {
				maxPayload = l - 2
			}
			delete(x.inBegin, idx)
			delete(x.inLen, idx)
			x.stateMu.Unlock()
		}
		n := min(len(p), maxPayload)
		continued := n < len(p)
		x.writeMu.Lock()
		var b []byte
		if x.experiment&variedRequestFeature != 0 {
			reqLen := maxPayload + 2
			b = make([]byte, 13+reqLen)
			frameVariedPiece(b, idx, begin, p[:n], continued, reqLen)
		} else {
			block := x.wireBlockSize()
			b = x.pieceFrame[:13+block]
			framePiece(b, r, p[:n], continued)
		}
		err := fullWrite(x.c, b)
		x.writeMu.Unlock()
		if err != nil {
			x.Close()
			return err
		}
		p = p[n:]
	}
	return nil
}

// SendBatch uses the credits already queued, flushing before waiting for more.
func (x *requestPieceCarrier) SendBatch(packets [][]byte) error {
	for _, p := range packets {
		if len(p) == 0 || len(p) > BlockSize-2 {
			return fmt.Errorf("invalid AWG datagram size: %d", len(p))
		}
	}
	x.sendMu.Lock()
	defer x.sendMu.Unlock()
	if x.experiment&variedRequestFeature != 0 {
		for _, p := range packets {
			if err := x.sendDatagram(p); err != nil {
				return err
			}
		}
		return nil
	}
	block := x.wireBlockSize()
	frameSize := 13 + block
	const chunk = 32
	for len(packets) > 0 {
		if len(packets[0]) > block-2 {
			if err := x.sendDatagram(packets[0]); err != nil {
				return err
			}
			packets = packets[1:]
			continue
		}
		var credits [chunk]request
		select {
		case credits[0] = <-x.requests:
		case <-x.done:
			return net.ErrClosed
		}
		n := 1
	collect:
		for n < min(len(packets), chunk) && len(packets[n]) <= block-2 {
			select {
			case credits[n] = <-x.requests:
				n++
			default:
				break collect
			}
		}
		x.writeMu.Lock()
		if x.pieceBatch == nil {
			x.pieceBatch = make([]byte, chunk*frameSize)
		}
		for i, p := range packets[:n] {
			framePiece(x.pieceBatch[i*frameSize:(i+1)*frameSize], credits[i], p, false)
		}
		err := fullWrite(x.c, x.pieceBatch[:n*frameSize])
		x.writeMu.Unlock()
		if err != nil {
			x.Close()
			return err
		}
		packets = packets[n:]
	}
	return nil
}

func framePiece(b []byte, r request, p []byte, continued bool) {
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	b[4] = 7
	binary.BigEndian.PutUint32(b[5:], uint32(r))
	length := uint16(len(p))
	if continued {
		length |= pieceContinuation
	}
	binary.BigEndian.PutUint16(b[13:], length)
	copy(b[15:], p)
	crand.Read(b[15+len(p):])
}

func frameVariedPiece(b []byte, idx, begin uint32, p []byte, continued bool, reqLen int) {
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	b[4] = 7
	binary.BigEndian.PutUint32(b[5:], idx)
	binary.BigEndian.PutUint32(b[9:], begin)
	length := uint16(len(p))
	if continued {
		length |= pieceContinuation
	}
	binary.BigEndian.PutUint16(b[13:], length)
	copy(b[15:], p)
	if pad := reqLen - (len(p) + 2); pad > 0 {
		crand.Read(b[15+len(p) : 15+len(p)+pad])
	}
}

// Invalid reports how many malformed wire messages were rejected.
func (x *requestPieceCarrier) Invalid() int64 { return x.invalid.Load() }

func (x *requestPieceCarrier) Close() {
	x.once.Do(func() {
		close(x.done)
		x.c.Close()
	})
}

func (x *requestPieceCarrier) Wait() {
	x.Close()
	x.wg.Wait()
}

func (x *requestPieceCarrier) Recv() <-chan []byte   { return x.received }
func (x *requestPieceCarrier) Done() <-chan struct{} { return x.done }
