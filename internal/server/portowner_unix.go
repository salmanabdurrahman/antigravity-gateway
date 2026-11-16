//go:build !windows

// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"fmt"
	"os"
	"os/exec"
)

// lsofCandidates lists the absolute lsof locations probed on POSIX systems. Absolute paths are used
// deliberately so a shadowed binary earlier in PATH can never be executed by daemon lifecycle code.
var lsofCandidates = []string{"/usr/sbin/lsof", "/usr/bin/lsof", "/bin/lsof"}

// processListeningOnPort resolves the process ID owning the listening TCP socket for port using
// the operating system socket table. It is used only as a recovery path when the PID file is
// missing, so lsof being unavailable is reported as ErrProcessNotFound rather than a crash.
func processListeningOnPort(port int) (int, error) {
	if port <= 0 {
		return 0, fmt.Errorf("%w: invalid port %d", ErrProcessNotFound, port)
	}

	ctx, cancel := portOwnerContext()
	defer cancel()

	pattern := fmt.Sprintf("tcp:%d", port)
	for _, candidate := range lsofCandidates {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}

		out, err := exec.CommandContext(ctx, candidate, "-nP", "-ti", pattern, "-sTCP:LISTEN").Output()
		if err != nil {
			continue
		}
		if pid := parseLsofPID(string(out)); pid > 0 {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("%w: port %d", ErrProcessNotFound, port)
}
