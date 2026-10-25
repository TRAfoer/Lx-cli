package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ownedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // Written before done closes.
}

// Ensure connects to an existing endpoint or starts an independent hidden mpv.
// The process does not inherit the caller's terminal streams or cancellation.
func (c *Client) Ensure(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := acquire(ctx, c.ensureGate); err != nil {
		return err
	}
	defer release(c.ensureGate)
	probe, probeCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err := c.connection(probe)
	probeCancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.endpoint == "" {
		return errors.New("mpv IPC endpoint is empty")
	}
	args, err := c.processArgs()
	if err != nil {
		return err
	}
	executable, err := executablePath()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, args...)
	prepareProcess(cmd)
	// Nil streams are connected to the null device by os/exec.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	process := &ownedProcess{cmd: cmd, done: make(chan struct{})}
	c.mu.Lock()
	c.process = process
	c.mu.Unlock()
	go func() { process.err = cmd.Wait(); close(process.done) }()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		probe, probeCancel = context.WithTimeout(ctx, 100*time.Millisecond)
		_, err = c.connection(probe)
		probeCancel()
		if err == nil {
			return nil
		}
		select {
		case <-process.done:
			if process.err == nil {
				return errors.New("mpv exited before IPC became ready")
			}
			return fmt.Errorf("mpv exited before IPC became ready: %w", process.err)
		case <-ctx.Done():
			_ = process.cmd.Process.Kill()
			return fmt.Errorf("wait for mpv IPC: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Close asks this endpoint to quit and reaps a process started by this Client.
// If a managed process does not respond before ctx expires, it is killed.
func (c *Client) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := acquire(ctx, c.ensureGate); err != nil {
		return err
	}
	defer release(c.ensureGate)
	c.mu.Lock()
	process := c.process
	c.mu.Unlock()
	s, connectErr := c.connection(ctx)
	var err error
	if connectErr == nil {
		_, err = c.Command(ctx, "quit")
	}
	if s != nil {
		s.shutdown(io.EOF)
	}
	if process == nil {
		// An absent endpoint is already closed; preserve cancellation errors.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			return err
		}
		return nil
	}
	select {
	case <-process.done:
		c.mu.Lock()
		if c.process == process {
			c.process = nil
		}
		c.mu.Unlock()
		return nil
	case <-ctx.Done():
		_ = process.cmd.Process.Kill()
		if err != nil {
			return err
		}
		return ctx.Err()
	}
}

func (c *Client) processArgs() ([]string, error) {
	args := make([]string, 0, len(c.args)+9)
	for i := 0; i < len(c.args); i++ {
		arg := c.args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("mpv extra arguments must be options: %q", arg)
		}
		if arg == "--input-ipc-server" {
			if i+1 < len(c.args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--input-ipc-server=") {
			continue
		}
		if strings.HasPrefix(arg, "--input-ipc-client") {
			return nil, errors.New("mpv input-ipc-client is not allowed for a dedicated player")
		}
		args = append(args, arg)
	}
	return append(args,
		"--no-config", "--idle=yes", "--no-video", "--terminal=no",
		"--input-terminal=no", "--force-window=no", "--audio-display=no",
		"--input-ipc-server="+c.endpoint,
	), nil
}

func executablePath() (string, error) {
	name := os.Getenv("ALX_MPV")
	if name == "" {
		name = defaultExecutable()
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find mpv (install it or set ALX_MPV): %w", err)
	}
	if strings.EqualFold(filepath.Ext(path), ".com") {
		gui := strings.TrimSuffix(path, filepath.Ext(path)) + ".exe"
		if info, statErr := os.Stat(gui); statErr == nil && !info.IsDir() {
			path = gui
		}
	}
	return path, nil
}
