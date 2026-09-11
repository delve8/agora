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

// The Web UI reads its Logto settings from the Server, so they must be derivable
// from the server-side environment: the issuer already implies the endpoint.
func TestResolveLogtoClientConfig(t *testing.T) {
	tests := []struct {
		name     string
		issuer   string
		endpoint string
		appID    string
		audience string
		want     LogtoClientConfig
		complete bool
	}{
		{
			name:     "endpoint comes from the issuer",
			issuer:   "https://logto.example.com/oidc",
			appID:    "spa-app",
			audience: "https://agora.example.com/api",
			want:     LogtoClientConfig{Endpoint: "https://logto.example.com", AppID: "spa-app", Audience: "https://agora.example.com/api"},
			complete: true,
		},
		{
			name:     "explicit endpoint wins",
			issuer:   "https://logto.example.com/oidc",
			endpoint: "https://login.example.com/",
			appID:    "spa-app",
			want:     LogtoClientConfig{Endpoint: "https://login.example.com", AppID: "spa-app"},
			complete: true,
		},
		{
			name:     "issuer without the oidc suffix is kept as is",
			issuer:   "https://logto.example.com",
			appID:    "spa-app",
			want:     LogtoClientConfig{Endpoint: "https://logto.example.com", AppID: "spa-app"},
			complete: true,
		},
		{
			name:     "no application id is incomplete",
			issuer:   "https://logto.example.com/oidc",
			audience: "https://agora.example.com/api",
			want:     LogtoClientConfig{Endpoint: "https://logto.example.com", Audience: "https://agora.example.com/api"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveLogtoClientConfig(test.issuer, test.endpoint, test.appID, test.audience)
			if got != test.want {
				t.Fatalf("ResolveLogtoClientConfig = %+v, want %+v", got, test.want)
			}
			if got.Complete() != test.complete {
				t.Fatalf("Complete() = %v, want %v", got.Complete(), test.complete)
			}
		})
	}
}
