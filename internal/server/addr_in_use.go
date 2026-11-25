// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"errors"
	"syscall"
)

// wsaAddrInUse is Windows' WSAEADDRINUSE errno. It is compared on every platform because
// syscall.EADDRINUSE is an invented value on Windows (APPLICATION_ERROR + iota) that never matches the
// errno a real bind failure carries, so a Windows bind conflict would otherwise skip the friendly
// "port is already in use" message, the owning PID, and the 'agw stop' recovery hint.
const wsaAddrInUse syscall.Errno = 10048

// isAddrInUse reports whether a listener error is an "address already in use" bind failure on any
// supported operating system. The original error is preserved for callers that wrap it further.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}

	// The bind failure is typically *net.OpError -> *os.SyscallError -> syscall.Errno, so the errno has
	// to be reached through the wrap chain instead of compared on the outermost error.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EADDRINUSE || errno == wsaAddrInUse
	}
	return false
}
