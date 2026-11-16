//go:build windows

// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"fmt"
	"os/exec"
)

// processListeningOnPort resolves the process ID owning the listening TCP socket for port by
// parsing the Windows TCP socket table. It is used only as a recovery path when the PID file is
// missing, so a missing netstat executable is reported as ErrProcessNotFound.
func processListeningOnPort(port int) (int, error) {
	if port <= 0 {
		return 0, fmt.Errorf("%w: invalid port %d", ErrProcessNotFound, port)
	}

	ctx, cancel := portOwnerContext()
	defer cancel()

	out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0, fmt.Errorf("%w: port %d: %v", ErrProcessNotFound, port, err)
	}

	if pid := parseNetstatPID(string(out), port); pid > 0 {
		return pid, nil
	}

	return 0, fmt.Errorf("%w: port %d", ErrProcessNotFound, port)
}
