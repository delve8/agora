package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDaemonIDPersistsUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("IDs are not stable UUIDs: %q and %q", first, second)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestResolveDaemonIDOverrideDoesNotRewriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	stored, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	override, err := ResolveDaemonID("test-daemon", path)
	if err != nil {
		t.Fatal(err)
	}
	if override != "test-daemon" {
		t.Fatalf("override = %q", override)
	}
	reloaded, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != stored {
		t.Fatalf("override rewrote config: %q != %q", reloaded, stored)
	}
}

func TestResolveDaemonIDRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"daemon_id":"../bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDaemonID("", path); err == nil {
		t.Fatal("expected invalid config error")
	}
	if _, err := ResolveDaemonID("bad/id", filepath.Join(t.TempDir(), "other.json")); err == nil {
		t.Fatal("expected invalid override error")
	}
}

func TestSaveDaemonIDPersistsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveDaemonID(path, "device-abc123"); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "device-abc123" {
		t.Fatalf("resolved id = %q, want device-abc123", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestSaveDaemonIDReplacesUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	stored, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveDaemonID(path, "device-xyz"); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDaemonID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if got == stored {
		t.Fatalf("SaveDaemonID did not replace the stored UUID %q", stored)
	}
	if got != "device-xyz" {
		t.Fatalf("resolved id = %q, want device-xyz", got)
	}
}

func TestSaveDaemonIDRejectsInvalid(t *testing.T) {
	if err := SaveDaemonID(filepath.Join(t.TempDir(), "config.json"), "bad/id"); err == nil {
		t.Fatal("expected invalid id error")
	}
	if err := SaveDaemonID(filepath.Join(t.TempDir(), "config.json"), ""); err == nil {
		t.Fatal("expected empty id error")
	}
}
