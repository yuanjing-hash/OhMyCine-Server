//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "OhMyCineNode"

func runPlatform() error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return errors.New("node_windows_service_detection_failed")
	}
	if isService {
		return svc.Run(windowsServiceName, nodeWindowsService{})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

type nodeWindowsService struct{}

func (nodeWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	statuses <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx) }()
	statuses <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-errCh:
			statuses <- svc.Status{State: svc.StopPending}
			if err != nil {
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending}
				cancel()
				err := <-errCh
				if err != nil {
					return false, 1
				}
				return false, 0
			default:
				statuses <- request.CurrentStatus
			}
		}
	}
}
