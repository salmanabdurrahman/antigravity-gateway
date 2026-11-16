// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"os"
	"testing"
)

// TestMain isolates the configuration root for the whole test binary so no test in this package can
// read or write the operator's real ~/.agw directory, even when an individual test forgets to
// override EnvHome.
func TestMain(m *testing.M) {
	tempHome, err := os.MkdirTemp("", "agw-config-test-home-")
	if err != nil {
		panic("creating isolated AGW_HOME for config tests: " + err.Error())
	}

	if envErr := os.Setenv(EnvHome, tempHome); envErr != nil {
		panic("setting isolated AGW_HOME for config tests: " + envErr.Error())
	}

	code := m.Run()

	if removeErr := os.RemoveAll(tempHome); removeErr != nil {
		panic("removing isolated AGW_HOME for config tests: " + removeErr.Error())
	}
	os.Exit(code)
}
