// Package webui contains the embedded static dashboard assets for local gateway monitoring.
package webui

import (
	_ "embed"
)

// IndexHTML holds the pre-compiled, self-contained dashboard HTML string.
//
//go:embed index.html
var IndexHTML string
