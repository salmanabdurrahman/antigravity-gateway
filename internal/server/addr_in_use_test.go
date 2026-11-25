// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestIsAddrInUseRecognizesBothErrnoFamilies verifies that the helper recognizes the POSIX
// EADDRINUSE as well as the Windows WSAEADDRINUSE errno, which syscall.EADDRINUSE never matches on
// Windows. The synthetic wrap chain mirrors the one produced by net.Listen.
func TestIsAddrInUseRecognizesBothErrnoFamilies(t *testing.T) {
	wrapErrno := func(errno syscall.Errno) error {
		return &net.OpError{
			Op:  "listen",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "bind", Err: errno},
		}
	}

	if !isAddrInUse(wrapErrno(syscall.EADDRINUSE)) {
		t.Error("expected POSIX EADDRINUSE to be recognized")
	}
	if !isAddrInUse(wrapErrno(wsaAddrInUse)) {
		t.Error("expected Windows WSAEADDRINUSE to be recognized on every platform")
	}
	if isAddrInUse(wrapErrno(syscall.EACCES)) {
		t.Error("unrelated errno must not be reported as an address-in-use failure")
	}
	if isAddrInUse(nil) {
		t.Error("nil error must not be reported as an address-in-use failure")
	}
	if isAddrInUse(errors.New("plain failure")) {
		t.Error("plain error must not be reported as an address-in-use failure")
	}
}

// TestIsAddrInUseRecognizesRealBindConflict verifies the helper against a genuine double bind on
// loopback, so the errno path is exercised without a synthetic error chain.
func TestIsAddrInUseRecognizesRealBindConflict(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding first listener: %v", err)
	}
	defer first.Close()

	addr := first.Addr().String()
	second, err := net.Listen("tcp", addr)
	if err == nil {
		_ = second.Close()
		t.Fatalf("expected the second bind on %s to fail", addr)
	}

	if !isAddrInUse(err) {
		t.Fatalf("expected a real bind conflict to be recognized, got %v", err)
	}
	if msg := fmt.Sprint(err); msg == "" {
		t.Error("expected the original bind error to stay printable")
	}
}
