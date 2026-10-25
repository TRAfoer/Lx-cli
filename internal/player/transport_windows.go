//go:build windows

package player

import (
	"context"
	"net"
	"os/exec"
	"syscall"

	"github.com/Microsoft/go-winio"
)

func dialIPC(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200}
}

func defaultExecutable() string { return "mpv.exe" }
