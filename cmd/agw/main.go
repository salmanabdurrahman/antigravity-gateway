// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"os"
)

func main() {
	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
