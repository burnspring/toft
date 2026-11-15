//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// startTimeout is how long a platform waits for an agent it started to answer.
const startTimeout = 10 * time.Second

// Ensure returns a client to the install's agent, starting one when nothing
// answers on the socket (ADR §129). The agent is the platform's own binary, from
// env — the allowlisted base environment, never the platform's own, which
// holds every secret the install has — in a session of its own, so it outlives
// the platform that started it.
//
// A running agent that speaks another protocol is reported, not replaced:
// replacing an agent is a re-exec, and the caller decides when.
func Ensure(ctx context.Context, dir, exe string, env []string, args ...string) (*Client, error) {
	c := NewClient(dir)
	if _, err := c.List(ctx); err == nil {
		return c, nil
	} else if pe := (*ProtocolError)(nil); errors.As(err, &pe) {
		return c, err
	}

	if err := os.MkdirAll(dir, 0o711); err != nil {
		return nil, fmt.Errorf("making the agent's directory: %w", err)
	}
	logFile, err := os.OpenFile(LogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the agent's log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(exe, append([]string{Command, "--dir", dir}, args...)...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("the shared-mode agent did not start: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("the shared-mode agent did not start: it exited (%v): %s", err, tailOf(LogPath(dir)))
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := c.List(ctx); err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("the shared-mode agent did not start: no answer on %s after %s: %s",
		SocketPath(dir), startTimeout, tailOf(LogPath(dir)))
}

// tailOf is the last lines of the agent's log, for the platform's refusal.
func tailOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, " | ")
}
