package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceCredentialRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.credential")
	if err := SaveDeviceCredential(path, "secret-value"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDeviceCredential(path)
	if err != nil || got != "secret-value" {
		t.Fatalf("credential = %q, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %o", info.Mode().Perm())
	}
}

func TestResolveServerAuthConfig(t *testing.T) {
	cfg, err := ResolveServerAuthConfig("", "", "", "")
	if err != nil || cfg.Mode != AuthModeLocal {
		t.Fatalf("default config = %+v, %v", cfg, err)
	}
	if _, err := ResolveServerAuthConfig(AuthModeLogto, "", "aud", ""); err == nil {
		t.Fatal("expected missing issuer error")
	}
	if err := ValidateServerAddress("0.0.0.0:8080", AuthModeLocal); err == nil {
		t.Fatal("expected non-loopback local address rejection")
	}
	if err := ValidateServerAddress("0.0.0.0:8080", AuthModeLogto); err != nil {
		t.Fatal(err)
	}
}
