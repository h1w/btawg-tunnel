// btawg-server runs the routed VPN hub, BitTorrent discovery, and the
// management panel. It supports install/run subcommands and a no-argument TUI.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"bittorrent-tunnel/btawg/internal/service"
)

func main() {
	if len(os.Args) < 2 {
		if err := tui(); err != nil {
			fmt.Fprintf(os.Stderr, "btawg-server: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cmd := os.Args[1]
	if cmd == "--version" {
		fmt.Println(service.Version)
		return
	}
	// Legacy foreground invocation with flags (e.g. -store, -no-tun) still works.
	if strings.HasPrefix(cmd, "-") {
		if err := runCmd(context.Background(), os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "btawg-server: %v\n", err)
			os.Exit(1)
		}
		return
	}

	switch cmd {
	case "install":
		if err := installCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "btawg-server: %v\n", err)
			os.Exit(1)
		}
	case "run":
		if err := runCmd(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "btawg-server: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "btawg-server: unknown command %q\n%s", cmd, usage)
		os.Exit(1)
	}
}

const usage = `Usage: btawg-server [install|run|tui] [flags]

Commands:
  install          copy binary, install/enable/start systemd unit
  run              run the hub in the foreground
  --version        print the executable version
  (no command)     open the interactive TUI

Legacy flags are accepted on the bare command or on run:
  -store PATH      bbolt database path
  -transport MODE  preferred mode for new imports
  -carrier-port N  override carrier listen port
  -external-ip IP  public IPv4 for native imports
  -dht-bootstrap   comma-separated bootstrap host:port list
  -no-tun          userspace overlay only
  -tun NAME        TUN interface name (default btawg0)
  -managed         use fixed ports, durable paths and systemd notify
  -subscription-host HOST  override the subscription host (default: public IPv4)
  -subscription-cert PATH  certificate chain PEM file
  -subscription-key PATH   private key PEM file
  -subscription-auto[=false] automatic IP TLS via HTTP-01 (on for managed hubs)
`
