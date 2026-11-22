package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/bind"
	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/dht"
	"bittorrent-tunnel/btawg/internal/direct"
	"bittorrent-tunnel/btawg/internal/iptunnel"
	"bittorrent-tunnel/btawg/internal/mesh"
)

type directPeer struct {
	secret     [32]byte
	until      atomic.Int64
	stop       context.CancelFunc
	candidates []mesh.Candidate
	ready      atomic.Bool
	done       chan struct{}
}

type peerOfferKey struct {
	ip, origin netip.Addr
	mode       string
}

func offerKey(p mesh.Pair) peerOfferKey { return peerOfferKey{p.IP, p.Origin, p.Mode} }

type directIdentity struct {
	private, public string
	tls             *iptunnel.Identity
}

func newDirectIdentity() (directIdentity, error) {
	private, public, err := awg.GenerateKeyPair()
	if err != nil {
		return directIdentity{}, err
	}
	tlsID, err := iptunnel.NewIdentity(private, iptunnel.Client)
	return directIdentity{private, public, tlsID}, err
}

func (n *meshNode) run(p2p, autoMap, allowPermanentUPnP bool, port uint16, endpoint, tcpEndpoint string, exitRunning *atomic.Int32) {
	var sockets *peerSockets
	var identity directIdentity
	if p2p && n.owner.shared != nil {
		var err error
		n.owner.shared.enableDirect(n.owner.cfg)
		identity, err = newDirectIdentity()
		if err == nil {
			sockets, err = openPeerSockets(n.ctx, port, autoMap, allowPermanentUPnP)
		}
		if err != nil {
			log.Printf("btawg-client: direct peer sockets: %v", err)
		}
	}
	if sockets != nil {
		defer sockets.close()
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	active := make(map[peerOfferKey]*directPeer)
	defer func() {
		for _, p := range active {
			p.stop()
		}
	}()
	requested := make(map[netip.Addr]time.Time)
	var observer netip.AddrPort
	var refresh time.Time
	for n.ctx.Err() == nil {
		reg := mesh.Registration{Egress: exitRunning.Load() > 0}
		if sockets != nil {
			p := n.peer.Load()
			reg.DirectKey, reg.TLSPin, reg.TransportPolicy = identity.public, identity.tls.Pin(), true
			reg.IncomingModes, reg.OutgoingModes = p.defaults.Incoming, p.defaults.Outgoing
			reg.PeerOverrides = p.meshOverrides
			now := time.Now()
			for ip, until := range requested {
				if !now.Before(until) || len(p.forPeer(ip).Outgoing) == 0 {
					delete(requested, ip)
				}
			}
			for range cap(n.owner.shared.directWanted) {
				select {
				case ip := <-n.owner.shared.directWanted:
					if len(p.forPeer(ip).Outgoing) != 0 && (len(requested) < mesh.MaxPeers || !requested[ip].IsZero()) {
						requested[ip] = now.Add(mesh.Lease * 2)
					}
				default:
					goto drained
				}
			}
		drained:
			for ip := range requested {
				reg.Want = append(reg.Want, ip)
			}
			slices.SortFunc(reg.Want, netip.Addr.Compare)
			if now.After(refresh) {
				if !observer.IsValid() {
					observer = n.observerEndpoint()
				}
				if observer.IsValid() {
					sockets.observe(n.ctx, observer)
				}
				refresh = time.Now().Add(8 * time.Second)
			}
			reg.Candidates = peerUnderlayCandidates(sockets.candidates(endpoint, tcpEndpoint, p.nativeIncoming, p.nativeOutgoing), netip.MustParseAddr(n.owner.cfg.ClientIP))
		}
		reply, err := n.register(reg)
		if p2p && n.owner.shared != nil && err == nil {
			n.owner.shared.setPeerNetwork(reply.Gateways, reply.PeerPrefixes)
		}
		if sockets != nil && (err == nil || err == errMeshDenied) {
			local := netip.MustParseAddr(n.owner.cfg.ClientIP)
			policies := n.peer.Load()
			offers := make(map[peerOfferKey]mesh.Pair, len(reply.Pairs))
			for _, offer := range reply.Pairs {
				if !offer.IP.Is4() || offer.IP == local || offer.Mode == "" || !offer.Origin.IsValid() || (offer.Origin != local && offer.Origin != offer.IP) || (offer.Origin == local) != offer.Initiator {
					continue
				}
				policy := policies.forPeer(offer.IP)
				modes := policy.Incoming
				if offer.Origin == local {
					modes = policy.Outgoing
				}
				if slices.Contains(modes, offer.Mode) {
					offers[offerKey(offer)] = offer
				}
			}
			for k, p := range active {
				select {
				case <-p.done:
					delete(active, k)
					continue
				default:
				}
				offer, ok := offers[k]
				if !ok || offer.Secret != p.secret || !p.ready.Load() && !slices.Equal(p.candidates, offer.Candidates) {
					p.stop()
					continue
				}
				p.until.Store(offer.Expires.UnixNano())
			}
			for k, offer := range offers {
				if len(active) >= mesh.MaxPeerOffers {
					break
				}
				if active[k] != nil {
					continue
				}
				ctx, stop := context.WithCancel(n.ctx)
				p := &directPeer{secret: offer.Secret, stop: stop, candidates: offer.Candidates, done: make(chan struct{})}
				p.until.Store(offer.Expires.UnixNano())
				active[k] = p
				workers.Go(func() { defer close(p.done); defer stop(); n.runPeer(ctx, sockets, identity, offer, p) })
			}
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(mesh.Poll):
		}
	}
}

func (n *meshNode) observerEndpoint() netip.AddrPort {
	cfg := n.owner.cfg
	if ep, err := netip.ParseAddrPort(cfg.CarrierEndpoint); err == nil {
		return ep
	}
	if cfg.InfoHash == "" {
		return netip.AddrPort{}
	}
	hash, err := awg.InfoHashBytes(cfg.InfoHash)
	if err != nil {
		return netip.AddrPort{}
	}
	ctx, cancel := context.WithTimeout(n.ctx, 900*time.Millisecond)
	defer cancel()
	if peers, err := dht.Discover(ctx, hash, cfg.DHTBootstrap); err == nil && len(peers) > 0 {
		return peers[0]
	}
	return netip.AddrPort{}
}

func (n *meshNode) runPeer(ctx context.Context, sockets *peerSockets, id directIdentity, p mesh.Pair, peer *directPeer) {
	started := time.Now()
	var tr carrier.Transport
	var raw net.Conn
	dialer := p.Initiator
	var err error
	if config.IsNativeMode(p.Mode) {
		tr, err = sockets.nativeConnect(ctx, p)
	} else {
		o := direct.Offer{Secret: p.Secret, Initiator: p.Initiator, Expires: p.Expires, Mode: p.Mode}
		for _, c := range p.Candidates {
			o.Candidates = append(o.Candidates, direct.Candidate{Addr: c.Addr, Kind: c.Kind, Protocol: c.Protocol})
		}
		tr, raw, dialer, err = sockets.stream.ConnectStream(ctx, o)
	}
	if err != nil {
		return
	}
	defer tr.Wait()
	defer tr.Close()
	linkCtx, stop := context.WithCancel(ctx)
	defer stop()
	closeCarrier := context.AfterFunc(linkCtx, tr.Close)
	defer closeCarrier()
	shared := n.owner.shared
	path := shared.newPath()
	path.directLife, path.directUntil = linkCtx, &peer.until
	path.peerOutbound, path.peerMode = p.Initiator, p.Mode
	path.peerHealth = &managedTunnel{c: &Client{path: path}, life: linkCtx}
	defer path.Close()
	defer shared.removeDirect(p.IP, path)
	local := netip.MustParseAddr(n.owner.cfg.ClientIP)
	link := &directTun{tunPath: path, local: local, peer: p.IP}
	var ready bool
	var probeDone chan struct{}
	defer func() {
		if probeDone != nil {
			stop()
			<-probeDone
		}
	}()
	publish := func() {
		if ready || !path.validDirect() {
			return
		}
		path.peerHealth.score.Store(int64(max(time.Since(started), time.Nanosecond)))
		shared.setDirect(p.IP, path)
		peer.ready.Store(true)
		ready = true
		if p.Initiator {
			probeDone = make(chan struct{})
			go func() { defer close(probeDone); n.probeDirect(linkCtx, path, p.IP) }()
		}
	}
	if config.IsTLSMode(p.Mode) {
		creds, err := id.tls.TLS(p.Key)
		if err != nil {
			return
		}
		stream := iptunnel.Stream(raw, tr)
		secure := tls.Server(stream, creds)
		if dialer {
			secure = tls.Client(stream, creds)
		}
		handshake, cancel := context.WithDeadline(linkCtx, p.Expires)
		stopHandshake := context.AfterFunc(handshake, tr.Close)
		err = secure.HandshakeContext(handshake)
		ok := stopHandshake()
		cancel()
		if err != nil || !ok {
			return
		}
		publish()
		var ioWG sync.WaitGroup
		ioWG.Go(func() {
			defer stop()
			_ = iptunnel.SendPackets(path, func(b []byte) error { _, err := secure.Write(b); return err })
		})
		ioWG.Go(func() { defer stop(); _ = iptunnel.ReceivePackets(link, secure) })
		n.waitPeer(linkCtx, tr, peer, nil)
		stop()
		path.Close()
		tr.Close()
		ioWG.Wait()
		return
	}
	var key device.NoisePublicKey
	if key.FromHex(p.Key) != nil {
		return
	}
	b := bind.New()
	level := device.LogLevelError
	if os.Getenv("BTAWG_AWG_VERBOSE") != "" {
		level = device.LogLevelVerbose
	}
	d := device.NewDevice(link, b, device.NewLogger(level, "btawg-p2p: "))
	defer d.Close()
	ep := bind.Endpoint(p.IP)
	profile := ""
	if p.Mode != config.ModeWG {
		profile = n.owner.cfg.Profile.String()
	}
	cfg := fmt.Sprintf("private_key=%s\n%spublic_key=%s\nallowed_ip=%s/32\nendpoint=%s\n", id.private, profile, p.Key, p.IP, ep.DstToString())
	if d.IpcSet(cfg) != nil || d.Up() != nil {
		return
	}
	b.Attach(ep, tr)
	if p.Initiator {
		if d.IpcSet(fmt.Sprintf("public_key=%s\npersistent_keepalive_interval=5\n", p.Key)) != nil {
			return
		}
		if remote := d.LookupPeer(key); remote != nil {
			remote.SendKeepalive()
		}
	}
	n.waitPeer(linkCtx, tr, peer, func() bool {
		if ready {
			return true
		}
		status, err := d.IpcGet()
		if err != nil {
			return false
		}
		for line := range strings.SplitSeq(status, "\n") {
			if sec, ok := strings.CutPrefix(line, "last_handshake_time_sec="); ok && sec != "0" {
				publish()
				return true
			}
		}
		return !time.Now().After(p.Expires)
	})
}

func (n *meshNode) waitPeer(ctx context.Context, tr carrier.Transport, p *directPeer, poll func() bool) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tr.Done():
			return
		case <-tick.C:
		}
		if time.Now().UnixNano() >= p.until.Load() || poll != nil && !poll() {
			return
		}
	}
}

// The paired path admits only packets authenticated under the peer's engine
// and stamped with its exact overlay address. Pin replies before feeding TUN.
type directTun struct {
	*tunPath
	local, peer netip.Addr
}

var _ tun.Device = (*directTun)(nil)

func (t *directTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		if len(b) < offset+20 {
			continue
		}
		p := b[offset:]
		h := int(p[0]&15) * 4
		if p[0]>>4 != 4 || h < 20 || h > len(p) || netip.AddrFrom4([4]byte(p[12:16])) != t.peer || netip.AddrFrom4([4]byte(p[16:20])) != t.local {
			continue
		}
		if windowsAssemblesIngress {
			p = t.inboundPacket(p)
			if p == nil {
				continue
			}
			h = int(p[0]&15) * 4
		}
		if !t.shared.acceptPeerPacket(t.tunPath, p, h) {
			continue
		}
		if windowsAssemblesIngress {
			batch := [1][]byte{p}
			if _, err := t.tunPath.Write(batch[:], 0); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := t.tunPath.Write([][]byte{b}, offset); err != nil {
			return 0, err
		}
	}
	return len(bufs), nil
}
