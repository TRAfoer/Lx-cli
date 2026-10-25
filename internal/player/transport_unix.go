//go:build !windows

package player

import (
	"context"
	"net"
	"os/exec"
	"syscall"
)

func dialIPC(ctx context.Context, endpoint string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", endpoint)
}

func prepareProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func defaultExecutable() string { return "mpv" }
