package browsercompanion

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

func terminateTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	hideWindow(kill)
	_ = kill.Run()
	_ = cmd.Process.Kill()
}
