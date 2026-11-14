// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// DefaultProviderID defines the standard provider key registered in client configurations.
const DefaultProviderID = "antigravity"

// DefaultProviderName defines the human-readable display name for the provider.
const DefaultProviderName = "Google Antigravity"

// BackupFile creates a timestamped copy of the specified file if it exists.
// Returns the absolute path of the backup, or empty string if the source does not exist.
func BackupFile(srcPath string, ts string) (string, error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat file %s: %w", srcPath, err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("source path %s is a directory, cannot backup", srcPath)
	}

	timestamp := cmp.Or(ts, time.Now().Format("20060102-150405"))
	backupPath := fmt.Sprintf("%s.bak-%s", srcPath, timestamp)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("opening source file %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	destFile, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating backup file %s: %w", backupPath, err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("copying backup data to %s: %w", backupPath, err)
	}

	if err := destFile.Sync(); err != nil {
		return "", fmt.Errorf("syncing backup file %s: %w", backupPath, err)
	}

	return backupPath, nil
}

// AtomicWriteJSON marshals data into formatted JSON and atomically writes it to destination.
func AtomicWriteJSON(dstPath string, data any, perm os.FileMode) error {
	dir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating parent directory %s: %w", dir, err)
	}

	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON payload: %w", err)
	}
	payload = append(payload, '\n')

	tempFile, err := os.CreateTemp(dir, "client-config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tempName := tempFile.Name()

	cleanup := func() {
		_ = os.Remove(tempName)
	}

	if _, err := tempFile.Write(payload); err != nil {
		tempFile.Close()
		cleanup()
		return fmt.Errorf("writing to temp file %s: %w", tempName, err)
	}

	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		cleanup()
		return fmt.Errorf("syncing temp file %s: %w", tempName, err)
	}

	if err := tempFile.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing temp file %s: %w", tempName, err)
	}

	if err := os.Chmod(tempName, perm); err != nil {
		cleanup()
		return fmt.Errorf("setting permissions on %s: %w", tempName, err)
	}

	if err := os.Rename(tempName, dstPath); err != nil {
		cleanup()
		return fmt.Errorf("renaming %s to %s: %w", tempName, dstPath, err)
	}

	return nil
}

var uppercaseWords = map[string]bool{
	"gpt": true,
	"oss": true,
	"glm": true,
	"ai":  true,
}

// PrettyModelName converts a hyphenated model identifier into a human-readable title.
// e.g. "gemini-3.1-pro-high" -> "Gemini 3.1 Pro High"
func PrettyModelName(id string) string {
	parts := strings.Split(id, "-")
	var result []string

	for i := 0; i < len(parts); i++ {
		p := parts[i]
		var next string
		if i+1 < len(parts) {
			next = parts[i+1]
		}

		// Combine single digits: 3-1 -> 3.1
		if isSingleDigit(p) && isSingleDigit(next) {
			result = append(result, fmt.Sprintf("%s.%s", p, next))
			i++
			continue
		}

		lower := strings.ToLower(p)
		if uppercaseWords[lower] || isParamSize(p) {
			result = append(result, strings.ToUpper(p))
		} else if len(p) > 0 {
			result = append(result, strings.ToUpper(p[:1])+p[1:])
		}
	}

	return strings.Join(result, " ")
}

// isSingleDigit checks if string s consists of exactly one decimal digit ('0' through '9').
func isSingleDigit(s string) bool {
	return len(s) == 1 && s[0] >= '0' && s[0] <= '9'
}

// isParamSize checks if string s matches a parameter size token (e.g. "120b", "70b").
func isParamSize(s string) bool {
	if len(s) < 2 {
		return false
	}
	last := s[len(s)-1]
	if last != 'b' && last != 'B' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ModelFilter matches model IDs against inclusion/exclusion glob patterns.
type ModelFilter struct {
	// includes holds compiled regexes for patterns that model IDs must match.
	includes []*regexp.Regexp

	// excludes holds compiled regexes for patterns that disqualify model IDs.
	excludes []*regexp.Regexp
}

// NewModelFilter compiles comma-separated patterns (e.g. "gemini-*,!*-low").
func NewModelFilter(pattern string) (*ModelFilter, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return &ModelFilter{}, nil
	}

	mf := &ModelFilter{}
	items := strings.Split(trimmed, ",")
	for _, item := range items {
		p := strings.TrimSpace(item)
		if p == "" {
			continue
		}
		isExclude := strings.HasPrefix(p, "!")
		raw := strings.TrimPrefix(p, "!")

		re, err := globToRegex(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}

		if isExclude {
			mf.excludes = append(mf.excludes, re)
		} else {
			mf.includes = append(mf.includes, re)
		}
	}

	return mf, nil
}

// Matches returns true if the model ID matches inclusion criteria and does not match exclusions.
func (mf *ModelFilter) Matches(id string) bool {
	if len(mf.includes) > 0 {
		matched := false
		for _, inc := range mf.includes {
			if inc.MatchString(id) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	for _, exc := range mf.excludes {
		if exc.MatchString(id) {
			return false
		}
	}

	return true
}

// globToRegex compiles a simple shell-style glob pattern containing '*' and '?' into a regular expression.
func globToRegex(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?i)^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// PreferredTestModels specifies the prioritized list of model names to test during verification.
var PreferredTestModels = []string{
	"gemini-3.1-pro-high",
	"gemini-3.8-flash-high",
	"gemini-3.8-flash-medium",
	"gemini-3.8-pro-high",
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"gpt-oss-120b-medium",
}

// PickTestModel selects the most appropriate model ID for end-to-end verification.
func PickTestModel(modelIDs []string) string {
	for _, pref := range PreferredTestModels {
		if slices.Contains(modelIDs, pref) {
			return pref
		}
	}
	if len(modelIDs) > 0 {
		return modelIDs[0]
	}
	return ""
}
