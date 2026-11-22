module bittorrent-tunnel/btawg

go 1.27.4

replace github.com/anacrolix/go-utp => ./third_party/go-utp

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/amnezia-vpn/amneziawg-go/v3 v3.1.20261116
	github.com/anacrolix/dht/v2 v2.24.0
	github.com/anacrolix/go-utp v0.0.0-00010101000000-000000000000
	github.com/anacrolix/torrent v1.48.1-0.20230103142631-c20f73d53e9f
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/grindlemire/go-tui v0.24.0
	go.etcd.io/bbolt v1.5.0
	golang.org/x/crypto v0.44.0
	golang.org/x/net v0.47.0
	golang.org/x/sys v0.50.0
	golang.org/x/term v0.48.0
)

require (
	github.com/alecthomas/atomic v0.1.0-alpha2 // indirect
	github.com/anacrolix/chansync v0.3.0 // indirect
	github.com/anacrolix/generics v0.0.0-20230816105729-c755655aee45 // indirect
	github.com/anacrolix/go-libutp v1.5.2-0.20260908013213-836dd42cdde6 // indirect
	github.com/anacrolix/log v0.15.2 // indirect
	github.com/anacrolix/missinggo v1.3.0 // indirect
	github.com/anacrolix/missinggo/perf v1.0.0 // indirect
	github.com/anacrolix/missinggo/v2 v2.10.0 // indirect
	github.com/anacrolix/mmsg v1.0.1 // indirect
	github.com/anacrolix/multiless v0.3.1-0.20221221005021-2d12701f83f7 // indirect
	github.com/anacrolix/stm v0.5.0 // indirect
	github.com/anacrolix/sync v0.4.0 // indirect
	github.com/benbjohnson/immutable v0.4.1-0.20221220213129-8932b999621d // indirect
	github.com/bradfitz/iter v0.0.0-20191230175014-e8f45d346db8 // indirect
	github.com/edsrzf/mmap-go v1.1.0 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/huandu/xstrings v1.3.2 // indirect
	github.com/rs/dnscache v0.0.0-20211102005908-e0241e321417 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/time v0.9.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gvisor.dev/gvisor v0.0.0-20231202080848-1f7806d17489 // indirect
)
