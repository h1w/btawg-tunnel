// btawg-client is the VPN client binary. It installs and manages its systemd
// service, runs the managed multi-mode daemon, or opens a TUI to toggle
// connection modes and view/rotate the shared SOCKS5 credentials. A bare
// import string still runs a single foreground connection for compatibility.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/service"
)

func main() {
	if handled, err := platformCommand(os.Args[1:]); handled {
		if err != nil {
			fatal(err)
		}
		return
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version":
			fmt.Println(service.Version)
			return
		case "install":
			if err := installMain(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "run":
			if err := runMain(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		}
	}
	if err := legacyOrTUI(os.Args[1:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "btawg-client: %v\n", err)
	os.Exit(1)
}

// installMain persists the client configuration and installs the service.
// The import secret is read from a file or a hidden prompt, never passed in
// service arguments.
func installMain(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	importFile := fs.String("import-file", "", "file containing the btawg:// import string (secret)")
	socksListen := fs.String("socks-listen", "127.0.0.1", "SOCKS5 listen IPv4 address")
	socksBase := fs.Int("socks-base-port", client.ManagedPortBase, "first port of the managed SOCKS5 block")
	noTun := fs.Bool("no-tun", false, "userspace netstack mode: no OS interface")
	tunName := fs.String("tun", "btawg0", "TUN interface name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !service.Privileged() {
		return fmt.Errorf("install requires an elevated terminal")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("use -import-file or the hidden terminal prompt")
	}
	_, err := client.LoadManagedConfig(client.ManagedConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := client.ValidateSOCKSBasePort(*socksBase); err != nil {
			return err
		}
		raw, err := readImport(*importFile)
		if err != nil {
			return err
		}
		if err := saveClientSettings(nil, raw, client.ManagedConfig{ListenIP: *socksListen, SOCKSBasePort: *socksBase, NoTUN: *noTun, TUNName: *tunName}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if fs.NFlag() != 0 {
		return fmt.Errorf("client already configured; run install without flags to upgrade")
	}
	if err := service.Client.Install(); err != nil {
		return err
	}
	fmt.Printf("btawg-client: installed and started; %s\n", installHelp())
	return nil
}

// readImport reads the import secret from a file or a hidden terminal prompt.
func readImport(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("pass -import-file FILE or run in a terminal to type the import")
	}
	fmt.Fprint(os.Stderr, "btawg-client: paste btawg:// import string: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// runMain runs either the managed daemon (`run -managed`, started by systemd)
// or a single foreground connection (`run <import>`).
func runMain(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	managed := fs.Bool("managed", false, "run as the managed multi-mode daemon")
	noTun := fs.Bool("no-tun", false, "userspace netstack mode")
	tunName := fs.String("tun", "btawg0", "TUN interface name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *managed {
		return runManaged(*noTun, *tunName)
	}
	raw := strings.TrimSpace(fs.Arg(0))
	if raw == "" {
		return fmt.Errorf("run: pass the btawg:// import string or -managed")
	}
	return runSingle(raw, *noTun, *tunName)
}

// legacyOrTUI runs a bare import string (single foreground connection) or the
// TUI when no import is given.
func legacyOrTUI(args []string) error {
	fs := flag.NewFlagSet("btawg-client", flag.ExitOnError)
	importFile := fs.String("config", "", "file containing the btawg:// import string")
	transport := fs.String("transport", "", "connection chain")
	dhtOnly := fs.Bool("dht", false, "discover the carrier through DHT")
	noTun := fs.Bool("no-tun", false, "userspace netstack mode")
	socksListen := fs.String("socks-listen", "127.0.0.1", "SOCKS5 listen IPv4 address")
	tunName := fs.String("tun", "btawg0", "TUN interface name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw := strings.TrimSpace(fs.Arg(0))
	if raw == "" && *importFile != "" {
		b, err := os.ReadFile(*importFile)
		if err != nil {
			return err
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw == "" {
		return runTUI()
	}
	if *transport != "" || *dhtOnly || *socksListen != "127.0.0.1" {
		return runSingleOpts(raw, *transport, *dhtOnly, *socksListen, *noTun, *tunName)
	}
	return runSingle(raw, *noTun, *tunName)
}

func runSingle(raw string, noTun bool, tunName string) error {
	return runSingleOpts(raw, "", false, "127.0.0.1", noTun, tunName)
}

// runSingleOpts keeps the original single-connection foreground behavior.
func runSingleOpts(raw, transport string, dhtOnly bool, socksListen string, noTun bool, tunName string) error {
	cfg, err := config.ParseImportString(raw)
	if err != nil {
		return err
	}
	listenIP, err := netip.ParseAddr(socksListen)
	if err != nil || !listenIP.Is4() {
		return fmt.Errorf("-socks-listen requires an IPv4 address")
	}
	cfg.SocksListenIP = listenIP
	if transport != "" {
		cfg.Transport = transport
	}
	cfg.Transport = config.CanonicalMode(cfg.Transport)
	if !config.ValidClientMode(cfg.Transport) {
		return fmt.Errorf("unsupported transport %q (want %s)", cfg.Transport, strings.Join(config.ClientModes(), "|"))
	}
	if dhtOnly {
		if !config.IsCarrierMode(cfg.Transport) {
			return fmt.Errorf("-dht is only available for carrier modes")
		}
		cfg.CarrierEndpoint = ""
	}

	var c *client.Client
	if noTun {
		c, err = client.New(cfg)
	} else {
		c, err = client.NewRealTUN(cfg, tunName)
	}
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.Run(ctx); err != nil {
		return err
	}
	return nil
}
