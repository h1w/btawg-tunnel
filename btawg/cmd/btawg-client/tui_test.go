package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
)

func TestPortRouteChoicesNeverOfferUnavailableRoutes(t *testing.T) {
	status := client.ControlResponse{
		SelectedRoutes: map[int]string{1080: "retired", 1092: "exit"},
		Routes:         []client.RouteOption{{ID: "exit", Name: "Renamed exit"}, {ID: "cascade", Name: "Via transit"}},
	}
	if got := routeLabel(status, 1080, true); !strings.Contains(got, "retired") || !strings.Contains(got, "unavailable") {
		t.Fatalf("stale selection displayed as %q", got)
	}
	if got := routeLabel(status, 1092, true); got != "Renamed exit" {
		t.Fatalf("renamed selection displayed as %q", got)
	}
	if got := routeLabel(status, 1091, true); !strings.Contains(got, "unavailable") {
		t.Fatalf("denied implicit hub displayed as %q", got)
	}
	ids := func(port int, running bool) []string {
		var ids []string
		for _, opt := range routeChoices(status, port, running) {
			ids = append(ids, opt.ID)
		}
		return ids
	}
	if got := ids(1080, true); !slices.Equal(got, []string{"exit", "cascade"}) {
		t.Fatalf("online choices = %v", got)
	}
	if got := ids(1080, false); !slices.Equal(got, []string{"hub"}) {
		t.Fatalf("offline choices = %v", got)
	}
	if got := ids(1081, false); len(got) != 0 {
		t.Fatalf("offline port already set to default has choices %v", got)
	}
}

func TestEverySOCKS5PortOffersRouteAndTransportControls(t *testing.T) {
	status := client.ControlResponse{
		Enabled:        []string{config.ModeWG},
		SelectedRoutes: map[int]string{1080: "exit", 1092: "exit"},
		Routes:         []client.RouteOption{{ID: "hub", Name: "Server"}, {ID: "exit", Name: "Exit"}},
	}
	actions := portActions(status, true)
	if len(actions) != 13 {
		t.Fatalf("port actions = %d, want 13", len(actions))
	}
	for i, action := range actions {
		port := 1080 + i
		want := "Server"
		if port == 1080 || port == 1092 {
			want = "Exit"
		}
		if !strings.Contains(action.Label, fmt.Sprint(port)) || !strings.HasSuffix(action.Label, " •  "+want) {
			t.Errorf("port %d main row: %q", port, action.Label)
		}
		screen, err := action.Run()
		if err != nil || screen == nil || len(screen.Choices) != 2 {
			t.Fatalf("port %d menu: %+v, %v", port, screen, err)
		}
		transportControl := screen.Choices[0].Label
		wantTransport := "Enable tunnel"
		if strings.Contains(action.Label, config.ModeWG) {
			wantTransport = "Disable tunnel"
		}
		if port >= 1090 {
			wantTransport = "Configure tunnels"
		}
		if transportControl != wantTransport {
			t.Errorf("port %d transport control: %q, want %q", port, transportControl, wantTransport)
		}
		if got := screen.Choices[1].Label; got != "Egress route: "+want {
			t.Errorf("port %d route control: %q", port, got)
		}
	}
}

func TestSOCKSMenuUsesConfiguredBase(t *testing.T) {
	status := client.ControlResponse{
		SOCKSBasePort:  11080,
		SelectedRoutes: map[int]string{11092: "exit"},
		Routes:         []client.RouteOption{{ID: "exit", Name: "Exit"}},
	}
	actions := portActions(status, true)
	for _, tc := range []struct {
		offset int
		port   string
	}{
		{0, "11080"}, {9, "11089"}, {10, "11090"}, {12, "11092"},
	} {
		screen, err := actions[tc.offset].Run()
		if err != nil || !strings.HasPrefix(screen.Title, "SOCKS5 "+tc.port+" — ") {
			t.Errorf("slot %d did not open the menu for port %s", tc.offset, tc.port)
		}
	}
	if !strings.HasSuffix(actions[12].Label, " •  Exit") {
		t.Fatalf("shifted balancer did not show its saved route: %q", actions[12].Label)
	}
}

func TestAdaptivePortDisplaysSelectedMode(t *testing.T) {
	status := client.ControlResponse{AdaptiveMode: "wg"}
	actions := portActions(status, true)
	if !strings.Contains(actions[len(actions)-1].Label, "wg") {
		t.Fatalf("selected adaptive mode missing: %s", actions[len(actions)-1].Label)
	}
}

func TestParsePeerPort(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  uint16
		valid bool
	}{
		{"0", 0, true}, {" 51840 ", 51840, true}, {"65535", 65535, true},
		{"65536", 0, false}, {"-1", 0, false}, {"UDP", 0, false},
	} {
		got, err := parsePeerPort(tc.input)
		if got != tc.want || (err == nil) != tc.valid {
			t.Errorf("parsePeerPort(%q) = %d, %v; want %d, valid=%v", tc.input, got, err, tc.want, tc.valid)
		}
	}
}
