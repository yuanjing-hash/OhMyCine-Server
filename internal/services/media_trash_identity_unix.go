//go:build !windows

package services

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func managedMediaFileIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("media is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("media file identity is unavailable")
	}
	return fmt.Sprintf("unix:%x:%x", stat.Dev, stat.Ino), nil
}
