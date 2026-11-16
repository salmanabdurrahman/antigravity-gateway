// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"os"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestMain isolates the configuration root for the whole test binary so no test in this package can
// read or write the operator's real ~/.agw directory, even when an individual test forgets to
// override AGW_HOME.
func TestMain(m *testing.M) {
	tempHome, err := os.MkdirTemp("", "agw-server-test-home-")
	if err != nil {
		panic("creating isolated AGW_HOME for server tests: " + err.Error())
	}

	if envErr := os.Setenv(config.EnvHome, tempHome); envErr != nil {
		panic("setting isolated AGW_HOME for server tests: " + envErr.Error())
	}

	code := m.Run()

	if removeErr := os.RemoveAll(tempHome); removeErr != nil {
		panic("removing isolated AGW_HOME for server tests: " + removeErr.Error())
	}
	os.Exit(code)
}
