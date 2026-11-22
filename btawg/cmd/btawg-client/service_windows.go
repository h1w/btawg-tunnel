package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/service"
)

func runManaged(noTun bool, tunName string) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if isService {
		return svc.Run("btawg-client", managedService{noTun, tunName})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runWindowsManaged(ctx, noTun, tunName, nil)
}

func runWindowsManaged(ctx context.Context, noTun bool, name string, ready func() error) error {
	unlock, err := service.LockManagedConfig(ctx)
	if err != nil {
		return err
	}
	cfg, err := client.LoadManagedConfig(client.ManagedConfigPath)
	unlock()
	if err != nil {
		return fmt.Errorf("load client configuration: %w", err)
	}
	if cfg.TUNName != "" {
		name = cfg.TUNName
	}
	return client.RunManaged(ctx, cfg, !noTun && !cfg.NoTUN, name, ready)
}

type managedService struct {
	noTun bool
	name  string
}

func (h managedService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	logFile, err := eventlog.Open("btawg-client")
	if err == nil {
		defer logFile.Close()
		log.SetOutput(windowsEventWriter{logFile})
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ready := make(chan struct{})
	finished := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 10000}
	statuses <- status
	go func() {
		finished <- runWindowsManaged(ctx, h.noTun, h.name, func() error { close(ready); return nil })
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-finished:
			if err != nil {
				if logFile != nil {
					_ = logFile.Error(1, err.Error())
				}
				return true, 1
			}
			return false, 0
		case <-ready:
			ready = nil
			if status.State != svc.StopPending {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
				log.Print("btawg-client: managed client ready")
			}
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				statuses <- status
			case svc.Stop, svc.Shutdown:
				if status.State != svc.StopPending {
					status = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
					statuses <- status
					stop()
				}
			}
		case <-tick.C:
			if status.State != svc.Running {
				status.CheckPoint++
				statuses <- status
			}
		}
	}
}

type windowsEventWriter struct{ event *eventlog.Log }

func (w windowsEventWriter) Write(p []byte) (int, error) {
	return len(p), w.event.Info(1, string(p))
}

func installHelp() string { return "reopen btawg-client.exe in elevated Windows Terminal" }

const setupRestartsService = true

func persistClientSettings(save func() error) error {
	state, _, err := service.Client.State()
	if err != nil {
		return err
	}
	wasRunning := state == "active" || state == "activating" || state == "reloading"
	if wasRunning {
		if err := service.Client.Control("stop"); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := service.LockManagedConfig(ctx)
	if err == nil {
		state, _, err = service.Client.State()
		if err == nil {
			if state == "inactive" || state == "failed" || state == service.NotInstalled {
				err = save()
			} else {
				err = fmt.Errorf("service changed to %s during settings save", state)
			}
		}
		unlock()
	}
	if wasRunning {
		return errors.Join(err, service.Client.Control("start"))
	}
	return err
}

func platformCommand(args []string) (bool, error) {
	if len(args) == 1 {
		switch args[0] {
		case "update-recovery":
			return true, service.RunWindowsUpdateRecovery()
		case "update-activate":
			return true, service.RunWindowsUpdateActivation()
		}
	}
	return false, nil
}
