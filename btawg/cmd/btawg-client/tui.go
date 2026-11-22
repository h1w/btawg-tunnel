package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/service"
)

func runTUI() error {
	return service.TUI(service.Menu{
		Unit: service.Client, Subtitle: "VPN client",
		Setup: func() (*service.Screen, error) { return clientSetup(false) },
		Install: func() (*service.Screen, error) {
			_, err := client.LoadManagedConfig(client.ManagedConfigPath)
			if errors.Is(err, os.ErrNotExist) {
				return clientSetup(true)
			}
			if err != nil {
				return nil, err
			}
			return nil, service.Client.Install()
		},
		Actions:              clientActions,
		SetupRestartsService: setupRestartsService,
	})
}

func clientSetup(install bool) (*service.Screen, error) {
	cfg, err := client.LoadManagedConfig(client.ManagedConfigPath)
	var previous *client.ManagedConfig
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg.ListenIP, cfg.TUNName = "127.0.0.1", "btawg0"
	case err != nil:
		return nil, err
	default:
		previous = &cfg
	}
	help := "Paste the btawg:// string from the server panel. The import is hidden."
	if previous != nil {
		help = "Leave the import blank to keep it. Saving changes restarts a running service."
	}
	help += " Permanent UPnP mappings may remain after a crash."
	submit := "Save settings"
	if install {
		submit = "Save and install"
	}
	return &service.Screen{
		Title: "Client settings", Text: help, Submit: submit,
		Fields: []service.Field{
			{Label: "VPN import", Secret: true},
			{Label: "SOCKS5 listen IPv4", Value: cfg.ListenIP},
			{Label: "TUN interface name", Value: cfg.TUNName},
			{Label: "Userspace only (no TUN)", Toggle: true, Checked: cfg.NoTUN},
			{Label: "Allow direct peer connections (P2P)", Toggle: true, Checked: cfg.P2P},
			{Label: "Automatically map the peer UDP port", Toggle: true, Checked: cfg.AutoMap},
			{Label: "Allow permanent UPnP mappings (e.g. MikroTik)", Toggle: true, Checked: cfg.AllowPermanentUPnP},
			{Label: "Allow serving assigned egress routes", Toggle: true, Checked: cfg.ServeEgress},
			{Label: "P2P listen port (0 = automatic)", Value: strconv.Itoa(int(cfg.PeerPort))},
			{Label: "Public P2P UDP address (optional)", Value: cfg.PeerEndpoint},
			{Label: "Public P2P TCP address (optional)", Value: cfg.PeerTCPEndpoint},
			{Label: "SOCKS5 base port", Value: strconv.Itoa(cfg.PortBase())},
		},
		Save: func(fields []service.Field) (*service.Screen, error) {
			port, err := parsePeerPort(fields[8].Value)
			if err != nil {
				return nil, err
			}
			base, err := strconv.Atoi(strings.TrimSpace(fields[11].Value))
			if err != nil {
				return nil, errors.New("SOCKS5 base port must be a number")
			}
			if err := client.ValidateSOCKSBasePort(base); err != nil {
				return nil, err
			}
			settings := client.ManagedConfig{
				ListenIP: fields[1].Value, TUNName: fields[2].Value, NoTUN: fields[3].Checked, SOCKSBasePort: base,
				P2P: fields[4].Checked, AutoMap: fields[5].Checked, AllowPermanentUPnP: fields[6].Checked, ServeEgress: fields[7].Checked,
				PeerPort: port, PeerEndpoint: fields[9].Value,
				PeerTCPEndpoint: fields[10].Value,
			}
			if err := persistClientSettings(func() error {
				latest, err := client.LoadManagedConfig(client.ManagedConfigPath)
				if err == nil {
					previous = &latest
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				} else {
					previous = nil
				}
				return saveClientSettings(previous, fields[0].Value, settings)
			}); err != nil {
				return nil, err
			}
			if install {
				return nil, service.Client.Install()
			}
			return nil, nil
		},
	}, nil
}

func parsePeerPort(raw string) (uint16, error) {
	port, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 16)
	if err != nil {
		return 0, errors.New("P2P listen port must be between 0 and 65535")
	}
	return uint16(port), nil
}

func saveClientSettings(previous *client.ManagedConfig, raw string, settings client.ManagedConfig) error {
	raw, listen, tun := strings.TrimSpace(raw), strings.TrimSpace(settings.ListenIP), strings.TrimSpace(settings.TUNName)
	ip, err := netip.ParseAddr(listen)
	if err != nil || !ip.Is4() {
		return errors.New("SOCKS5 listen address must be IPv4")
	}
	if !settings.NoTUN {
		if err := service.ValidateInterface(tun, 15); err != nil {
			return err
		}
	}
	var next client.ManagedConfig
	if previous != nil {
		next = *previous
	}
	if raw != "" || previous == nil {
		parsed, err := config.ParseImportString(raw)
		if err != nil {
			return errors.New("invalid VPN import: paste the complete btawg:// string from the server panel")
		}
		mode := config.CanonicalMode(parsed.Transport)
		if !config.ValidClientMode(mode) {
			return errors.New("VPN import uses an unsupported transport")
		}
		if next.Import != raw {
			next.Import, next.Enabled = raw, []string{mode}
		}
	}
	if previous == nil {
		next.Credentials, err = client.GenerateCredentials()
		if err != nil {
			return err
		}
	}
	next.ListenIP, next.TUNName, next.NoTUN = ip.String(), tun, settings.NoTUN
	next.SOCKSBasePort = settings.SOCKSBasePort
	next.P2P, next.AutoMap, next.ServeEgress = settings.P2P, settings.AutoMap, settings.ServeEgress
	next.AllowPermanentUPnP = settings.AllowPermanentUPnP && settings.AutoMap
	next.PeerPort, next.PeerEndpoint = settings.PeerPort, strings.TrimSpace(settings.PeerEndpoint)
	next.PeerTCPEndpoint = strings.TrimSpace(settings.PeerTCPEndpoint)
	return next.Save(client.ManagedConfigPath)
}

func clientActions(_ string) ([]service.Action, error) {
	status, running, err := client.RequestControl(client.ControlRequest{Op: "status"})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return append(portActions(status, running),
		service.Action{Label: "Peer connections", Run: func() (*service.Screen, error) { return peerSettingsScreen(client.RequestControl) }},
		service.Action{Label: "Show SOCKS5 credentials", Run: func() (*service.Screen, error) { return socksCredentials("show") }},
		service.Action{Label: "Rotate SOCKS5 credentials", Confirm: "Replace the login and password for all ports? Existing SOCKS5 sessions will close.", Run: func() (*service.Screen, error) { return socksCredentials("rotate") }},
	), nil
}

func portActions(status client.ControlResponse, running bool) []service.Action {
	modes := config.ClientModes()
	policies := client.BalancerPorts()
	actions := make([]service.Action, 0, len(modes)+len(policies)+2)
	base := status.PortBase()
	for i, mode := range modes {
		port := base + i
		checked, state := "[ ]", "off"
		if running {
			state = "reserved"
		}
		enabled := slices.Contains(status.Enabled, mode)
		if enabled {
			checked, state = "[x]", "enabled on start"
			if running {
				state = "enabled"
			}
		}
		actions = append(actions, service.Action{
			Label: fmt.Sprintf("%s %d  %-22s %s  •  %s", checked, port, mode, state, routeLabel(status, port, running)),
			Run: func() (*service.Screen, error) {
				toggle := "Enable tunnel"
				if enabled {
					toggle = "Disable tunnel"
				}
				return &service.Screen{
					Title: fmt.Sprintf("SOCKS5 %d — %s", port, mode),
					Choices: []service.Action{
						{Label: toggle, Run: func() (*service.Screen, error) {
							_, _, err := client.RequestControl(client.ControlRequest{Op: "toggle", Mode: mode})
							return nil, err
						}},
						routeAction(port, status, running),
					},
				}, nil
			},
		})
	}
	labels := [...]string{"Failover", "Round-robin", "Adaptive"}
	for i, policy := range policies {
		port, label := base+len(modes)+i, labels[i]
		pool, custom := status.Balancers[policy]
		if !custom {
			pool = status.Enabled
		}
		actions = append(actions, service.Action{
			Label: fmt.Sprintf("    %d  %-22s %d/%d tunnels  •  %s", port, label, len(pool), len(modes), routeLabel(status, port, running)),
			Run: func() (*service.Screen, error) {
				return &service.Screen{
					Title: fmt.Sprintf("SOCKS5 %d — %s", port, label),
					Choices: []service.Action{
						{Label: "Configure tunnels", Run: func() (*service.Screen, error) { return balancerSettings(port, label, policy) }},
						routeAction(port, status, running),
					},
				}, nil
			},
		})
	}
	return actions
}

func routeLabel(status client.ControlResponse, port int, running bool) string {
	id := status.RouteID(port)
	if i := slices.IndexFunc(status.Routes, func(opt client.RouteOption) bool { return opt.ID == id }); i >= 0 {
		return status.Routes[i].Name
	}
	if id == client.HubRouteID {
		id = "Server default"
	}
	if running {
		return "unavailable: " + id
	}
	return "offline: " + id
}

func routeChoices(status client.ControlResponse, port int, running bool) []client.RouteOption {
	if running {
		return status.Routes
	}
	if status.RouteID(port) != client.HubRouteID {
		return []client.RouteOption{{ID: client.HubRouteID, Name: "Reset to server default"}}
	}
	return nil
}

func routeAction(port int, status client.ControlResponse, running bool) service.Action {
	return service.Action{
		Label: "Egress route: " + routeLabel(status, port, running),
		Run: func() (*service.Screen, error) {
			status, running, err := client.RequestControl(client.ControlRequest{Op: "status"})
			if err != nil {
				return nil, err
			}
			opts := routeChoices(status, port, running)
			s := &service.Screen{
				Title: fmt.Sprintf("SOCKS5 %d — Egress route", port),
				Text:  "Selected: " + routeLabel(status, port, running),
			}
			switch {
			case !running:
				s.Text += "\nStart the service to see permitted routes."
			case len(opts) == 0:
				s.Text += "\nNo permitted routes are currently available."
			}
			s.Choices = make([]service.Action, 0, len(opts))
			selected := status.RouteID(port)
			for _, opt := range opts {
				name := opt.Name
				if running {
					name += " (" + opt.ID + ")"
				}
				checked := "[ ] "
				if opt.ID == selected {
					checked = "[x] "
				}
				s.Choices = append(s.Choices, service.Action{
					Label: checked + name,
					Run: func() (*service.Screen, error) {
						_, _, err := client.RequestControl(client.ControlRequest{Op: "set-route", Port: port, RouteID: opt.ID})
						return nil, err
					},
				})
			}
			return s, nil
		},
	}
}

func balancerSettings(port int, label, policy string) (*service.Screen, error) {
	status, _, err := client.RequestControl(client.ControlRequest{Op: "status"})
	if err != nil {
		return nil, err
	}
	pool, custom := status.Balancers[policy]
	if !custom {
		pool = status.Enabled
	}
	modes := config.ClientModes()
	reorder := policy == "failover" || policy == "adaptive"
	fields := make([]service.Field, 0, len(modes))
	text := "Choose tunnels for this port only. An empty list disables forwarding."
	if reorder {
		text = "First checked working tunnel wins. Use - to move up, + or = to move down."
		if policy == "adaptive" {
			text = "Comparable paths follow this order; a consistently faster path can win. Use - to move up, + or = to move down."
		}
		if custom {
			for _, mode := range pool {
				fields = append(fields, service.Field{Label: mode, Toggle: true, Checked: true})
			}
		}
	}
	for _, mode := range modes {
		checked := slices.Contains(pool, mode)
		if reorder && custom && checked {
			continue
		}
		fields = append(fields, service.Field{Label: mode, Toggle: true, Checked: checked})
	}
	return &service.Screen{
		Title:   fmt.Sprintf("SOCKS5 %d — %s", port, label),
		Text:    text,
		Submit:  "Save tunnel selection",
		Fields:  fields,
		Reorder: reorder,
		Save: func(fields []service.Field) (*service.Screen, error) {
			selected := make([]string, 0, len(fields))
			for _, field := range fields {
				if field.Checked {
					selected = append(selected, field.Label)
				}
			}
			_, _, err := client.RequestControl(client.ControlRequest{Op: "set-balancer", Balancer: policy, Enabled: selected})
			return nil, err
		},
	}, nil
}

func socksCredentials(op string) (*service.Screen, error) {
	r, _, err := client.RequestControl(client.ControlRequest{Op: op})
	if err != nil {
		return nil, err
	}
	if r.Credentials == nil {
		return nil, errors.New("SOCKS5 credentials not available")
	}
	title := "SOCKS5 credentials"
	if op == "rotate" {
		title = "New SOCKS5 credentials"
	}
	return service.SOCKSCredentialsScreen(title, r.Credentials.Username, r.Credentials.Password), nil
}
