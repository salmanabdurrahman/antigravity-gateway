// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// PortOwnerLookupTimeout bounds how long the platform socket table query may take, so daemon
// lifecycle commands never hang on a stuck lsof or netstat invocation.
const PortOwnerLookupTimeout = 2 * time.Second

// parseLsofPID extracts the first positive process ID from newline separated lsof output.
func parseLsofPID(output string) int {
	for line := range strings.SplitSeq(output, "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// parseNetstatPID extracts the process ID of the LISTENING socket on port from Windows netstat
// output. Parsing lives here rather than in the Windows-only file so the format handling stays
// unit tested on every platform.
func parseNetstatPID(output string, port int) int {
	suffix := ":" + strconv.Itoa(port)
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if !strings.HasSuffix(fields[1], suffix) {
			continue
		}

		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// portOwnerContext builds the bounded context used for platform socket table lookups.
func portOwnerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), PortOwnerLookupTimeout)
}
