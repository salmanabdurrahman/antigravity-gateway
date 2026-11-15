//go:build windows

// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"errors"
	"os"
	"syscall"
)

// IsProcessAlive checks whether the specified process ID currently exists on Windows platforms
// by querying the process handle exit code. If access is denied, the process exists under
// another account or privilege level.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return true
		}
		return false
	}
	defer syscall.CloseHandle(h)

	var exitCode uint32
	if err := syscall.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}

// detachedSysProcAttr returns Windows-specific process attributes for detached background execution,
// setting creation flags to decouple the process from the parent console.
func detachedSysProcAttr() *syscall.SysProcAttr {
	const (
		detachedProcess     = 0x00000008
		createNewProcessGrp = 0x00000200
	)
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGrp,
	}
}

// terminateProcess terminates the process handle on Windows platforms.
func terminateProcess(proc *os.Process) error {
	return proc.Kill()
}

// isProcessDoneErr checks whether the error indicates the process has already terminated.
func isProcessDoneErr(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
