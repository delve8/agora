package config

import (
	"path/filepath"
	"testing"
)

func TestServerURLRemembersPairingTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveDaemonID(path, "device-1"); err != nil {
		t.Fatal(err)
	}
	// A trailing slash is normalised so later comparisons and URL joins are
	// predictable.
	if err := SaveServerURL(path, "https://agora.example.com/"); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveServerURL(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://agora.example.com" {
		t.Fatalf("resolved server url = %q", got)
	}
	// Saving the server URL must not clobber the paired device id.
	id, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if id != "device-1" {
		t.Fatalf("daemon id = %q, want device-1", id)
	}
}

func TestSaveServerURLRejectsEmpty(t *testing.T) {
	if err := SaveServerURL(filepath.Join(t.TempDir(), "config.json"), "  "); err == nil {
		t.Fatal("expected empty server url error")
	}
}

func TestResolveServerURLToleratesMissingFile(t *testing.T) {
	value, err := ResolveServerURL(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil && value != "" {
		t.Fatalf("expected empty value for a missing config, got %q", value)
	}
}

// An empty path means "the default config file"; both savers must resolve it
// before writing, otherwise pairing silently fails to record the server URL.
func TestSaveWithDefaultPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SaveDaemonID("", "device-home"); err != nil {
		t.Fatal(err)
	}
	if err := SaveServerURL("", "https://agora.example.com"); err != nil {
		t.Fatal(err)
	}
	id, err := ResolveDaemonID("", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "device-home" {
		t.Fatalf("daemon id = %q, want device-home", id)
	}
	url, err := ResolveServerURL("")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://agora.example.com" {
		t.Fatalf("server url = %q", url)
	}
}
