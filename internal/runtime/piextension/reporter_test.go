package piextension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The extension is injected with `pi -e <path>`, so it must exist on disk, be
// private to the user, and stay current when Agora ships a new version.
func TestReporterPathInstallsCurrentExtension(t *testing.T) {
	home := t.TempDir()
	path, err := ReporterPath(home)
	if err != nil {
		t.Fatalf("ReporterPath: %v", err)
	}
	if want := filepath.Join(home, ".agora", "extensions", "pi", ReporterFileName); path != want {
		t.Fatalf("ReporterPath = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat extension: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("extension permissions = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The extension must subscribe to the provider events that make rebind
	// exact, and must identify its Host.
	for _, want := range []string{"session_before_switch", "session_start", "AGORA_HOST_ID", "session.report"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("extension is missing %q", want)
		}
	}

	// A second call must not rewrite an up-to-date extension.
	if err := os.Chtimes(path, time.Now(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReporterPath(home); err != nil {
		t.Fatalf("second ReporterPath: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("up-to-date extension was rewritten: %s -> %s", before.ModTime(), after.ModTime())
	}
}

// A stale extension must be replaced, otherwise an old Agora version would keep
// reporting through an outdated protocol.
func TestReporterPathReplacesStaleExtension(t *testing.T) {
	home := t.TempDir()
	path, err := ReporterPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("// stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReporterPath(home); err != nil {
		t.Fatalf("ReporterPath: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "stale") || !strings.Contains(string(body), "session_before_switch") {
		t.Fatal("stale extension was not replaced")
	}
}
