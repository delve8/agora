// Package piextension ships the Pi extension Agora injects into managed Agent
// processes.
package piextension

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed agora-session-reporter.ts
var reporterSource []byte

// ReporterFileName is the file name of the injected extension.
const ReporterFileName = "agora-session-reporter.ts"

// ReporterPath materializes the Agora session reporter under the user's Agora
// directory and returns its path.
//
// The extension is injected with `pi -e <path>`, which works even when the user
// passes `--no-extensions`: that flag disables extension discovery, not explicit
// paths. Writing it under ~/.agora keeps it out of the user's own extension
// directories, so Agora never shows up as a project-local or global extension.
//
// The write is atomic and skipped when the content is already current.
func ReporterPath(homeDir string) (string, error) {
	if homeDir == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for the Pi reporter extension: %w", err)
		}
		homeDir = resolved
	}
	dir := filepath.Join(homeDir, ".agora", "extensions", "pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Pi extension directory: %w", err)
	}
	path := filepath.Join(dir, ReporterFileName)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, reporterSource) {
		return path, nil
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, reporterSource, 0o600); err != nil {
		return "", fmt.Errorf("write Pi reporter extension: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("install Pi reporter extension: %w", err)
	}
	return path, nil
}
