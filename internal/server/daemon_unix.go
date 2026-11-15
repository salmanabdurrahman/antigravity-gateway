//go:build !windows

// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"errors"
	"os"
	"syscall"
)

// IsProcessAlive checks whether the specified process ID currently exists in the operating system.
// On Unix platforms, it first attempts to reap child zombie processes using Wait4, then checks
// active existence via signal 0 delivery.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	// First check if target process is a child of the current process to reap any terminated zombie.
	var status syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err == nil {
		if wpid == pid {
			// Child process has exited and has now been reaped.
			return false
		}
		if wpid == 0 {
			// Child process is still actively running.
			return true
		}
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Sending signal 0 checks for process existence without actually delivering a signal.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}

	// If errno is EPERM, process exists but belongs to another user.
	if errors.Is(err, syscall.EPERM) {
		return true
	}

	return false
}

// detachedSysProcAttr returns Unix-specific process attributes for detached daemon execution,
// initiating a new session via Setsid to decouple the child from the parent terminal.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess delivers SIGTERM to allow graceful daemon shutdown on Unix platforms.
func terminateProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// isProcessDoneErr checks whether the error indicates the process has already terminated.
func isProcessDoneErr(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
