package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"bittorrent-tunnel/btawg/internal/service"
)

func tui() error {
	return service.TUI(service.Menu{
		Unit: service.Server, Subtitle: "VPN hub",
		Setup: serverSetup,
		Install: func() (*service.Screen, error) {
			launch, err := loadServerLaunch()
			if err != nil {
				return nil, err
			}
			if err := installServer(launch); err != nil {
				return nil, err
			}
			return panelScreen()
		},
		Actions: func(string) ([]service.Action, error) {
			return []service.Action{{Label: "Web panel and initial login", Run: panelScreen}}, nil
		},
	})
}

func serverSetup() (*service.Screen, error) {
	launch, err := loadServerLaunch()
	if err != nil {
		return nil, err
	}
	return &service.Screen{
		Title: "Server settings", Submit: "Save settings",
		Text: "Saving changes restarts a running service. Manage groups, gateways, clients and routing in the web panel.",
		Fields: []service.Field{
			{Label: "Database path", Value: launch.Store},
			{Label: "TUN interface", Value: launch.TUNName},
			{Label: "Userspace only (no TUN)", Toggle: true, Checked: launch.NoTUN},
		},
		Save: func(fields []service.Field) (*service.Screen, error) {
			return nil, (serverLaunch{Store: strings.TrimSpace(fields[0].Value), TUNName: strings.TrimSpace(fields[1].Value), NoTUN: fields[2].Checked}).save()
		},
	}, nil
}

func panelScreen() (*service.Screen, error) {
	launch, err := loadServerLaunch()
	if err != nil {
		return nil, err
	}
	path := launch.pendingPath()
	pending, err := os.ReadFile(path)
	s := &service.Screen{
		Title: "Web panel",
		Text:  "Username:\nadmin",
	}
	const access = "\n\nDefault local URL: http://127.0.0.1:8080\n" +
		"VPN URL: the Default group gateway (default http://10.77.0.1:8080).\n" +
		"Before the VPN is connected: ssh -N -L 8080:127.0.0.1:8080 USER@VPS"
	if errors.Is(err, os.ErrNotExist) {
		s.Text += "\nUse the password saved at installation." + access
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.Title = "Initial web panel login"
	s.Text += fmt.Sprintf("\nPassword:\n%s\n\nSave this password now. Enter acknowledges it; Esc keeps it for later.", pending) + access
	s.Dismiss = func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return s, nil
}
