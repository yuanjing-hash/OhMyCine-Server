//go:build windows

package updater

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

func processExecutable(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func waitForProcessExit(ctx context.Context, pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	for {
		result, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			return err
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
