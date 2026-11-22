package client

import (
	"errors"
	"sync"

	"bittorrent-tunnel/btawg/internal/tunsetup"
	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
)

type underlayBind struct {
	conn.Bind
	lifecycle      sync.Mutex
	mu             sync.Mutex
	index4, index6 uint32
	luid4, luid6   uint64
}

func newUnderlayBind() conn.Bind { return &underlayBind{Bind: conn.NewStdNetBind()} }

func (b *underlayBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	fns, actual, err := b.Bind.Open(port)
	if err == nil {
		b.index4, b.index6, b.luid4, b.luid6 = 0, 0, 0, 0
	}
	return fns, actual, err
}

func (b *underlayBind) Close() error {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	err := b.Bind.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.index4, b.index6, b.luid4, b.luid6 = 0, 0, 0, 0
	return err
}

func (b *underlayBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	ip := ep.DstIP().Unmap()
	b.mu.Lock()
	defer b.mu.Unlock()
	index, luid, err := tunsetup.UnderlayRouteIdentity(ip)
	if err != nil {
		return err
	}
	binder, ok := b.Bind.(conn.BindSocketToInterface)
	if !ok {
		return errors.New("native bind cannot select the underlay interface")
	}
	indexp, luidp := &b.index4, &b.luid4
	if ip.Is6() {
		indexp, luidp = &b.index6, &b.luid6
	}
	if *indexp != index || *luidp != luid {
		if ip.Is4() {
			err = binder.BindSocketToInterface4(index, false)
		} else {
			err = binder.BindSocketToInterface6(index, false)
		}
		if err != nil {
			return err
		}
		*indexp, *luidp = index, luid
	}
	ep.ClearSrc()
	return b.Bind.Send(bufs, ep)
}
