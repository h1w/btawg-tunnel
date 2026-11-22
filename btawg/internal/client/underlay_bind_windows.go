package client

import (
	"errors"
	"sync"

	"bittorrent-tunnel/btawg/internal/tunsetup"
	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
)

type underlayBind struct {
	conn.Bind
	mu sync.Mutex
}

func newUnderlayBind() conn.Bind { return &underlayBind{Bind: conn.NewStdNetBind()} }

func (b *underlayBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	ip := ep.DstIP().Unmap()
	b.mu.Lock()
	defer b.mu.Unlock()
	index, err := tunsetup.UnderlayInterface(ip)
	if err != nil {
		return err
	}
	binder, ok := b.Bind.(conn.BindSocketToInterface)
	if !ok {
		return errors.New("native bind cannot select the underlay interface")
	}
	if ip.Is4() {
		err = binder.BindSocketToInterface4(index, false)
	} else {
		err = binder.BindSocketToInterface6(index, false)
	}
	if err != nil {
		return err
	}
	ep.ClearSrc()
	return b.Bind.Send(bufs, ep)
}
