package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/service"
)

func runManaged(noTun bool, tunName string) error {
	mc, err := client.LoadManagedConfig(client.ManagedConfigPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", client.ManagedConfigPath, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if mc.TUNName != "" {
		tunName = mc.TUNName
	}
	return client.RunManaged(ctx, mc, !noTun && !mc.NoTUN, tunName, service.NotifyReady)
}

func installHelp() string { return "use sudo btawg-client to view SOCKS5 credentials" }

const setupRestartsService = false

func persistClientSettings(save func() error) error { return save() }

func persistSubscriptionProfile(save func() error) error {
	state, _, err := service.Client.State()
	if err != nil {
		return err
	}
	if state != "active" && state != "activating" && state != "reloading" {
		return save()
	}
	// Stop the daemon before writing: its control socket also saves settings.
	if err := service.Client.Control("stop"); err != nil {
		return err
	}
	return errors.Join(save(), service.Client.Control("start"))
}

func platformCommand([]string) (bool, error) { return false, nil }
