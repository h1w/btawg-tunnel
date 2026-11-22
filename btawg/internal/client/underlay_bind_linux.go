package client

import "github.com/amnezia-vpn/amneziawg-go/v3/conn"

func newUnderlayBind() conn.Bind { return conn.NewDefaultBind() }
