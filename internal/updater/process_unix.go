//go:build !windows

package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func processExecutable(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}

func waitForProcessExit(ctx context.Context, pid int) error {
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
