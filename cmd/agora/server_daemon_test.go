package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonWSURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "http", input: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/api/daemon/ws"},
		{name: "https", input: "https://agora.example.com/", want: "wss://agora.example.com/api/daemon/ws"},
		{name: "ws", input: "ws://127.0.0.1:8080", want: "ws://127.0.0.1:8080/api/daemon/ws"},
		{name: "wss", input: "wss://agora.example.com/", want: "wss://agora.example.com/api/daemon/ws"},
		{name: "existing-path", input: "http://127.0.0.1:8080/api/daemon/ws", want: "ws://127.0.0.1:8080/api/daemon/ws"},
		{name: "existing-ws-path", input: "wss://agora.example.com/api/daemon/ws/", want: "wss://agora.example.com/api/daemon/ws"},
		{name: "host", input: "127.0.0.1:8080", want: "ws://127.0.0.1:8080/api/daemon/ws"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := daemonWSURL(test.input); got != test.want {
				t.Fatalf("daemonWSURL(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestPairDeviceReturnsDeviceIDAndCredential(t *testing.T) {
	var gotCode, gotName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/pair" {
			t.Errorf("pair request path = %q, want /api/daemon/pair", r.URL.Path)
		}
		var body struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode pair request: %v", err)
		}
		gotCode, gotName = body.Code, body.Name
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"device-pair-1","credential":"topsecret"}`))
	}))
	defer server.Close()

	deviceID, credential, err := pairDevice(server.URL, "code-42", "workstation")
	if err != nil {
		t.Fatal(err)
	}
	if gotCode != "code-42" || gotName != "workstation" {
		t.Fatalf("pair request carried code=%q name=%q, want code-42/workstation", gotCode, gotName)
	}
	if deviceID != "device-pair-1" {
		t.Fatalf("device id = %q, want device-pair-1", deviceID)
	}
	if credential != "topsecret" {
		t.Fatalf("credential = %q, want topsecret", credential)
	}
}

func TestPairDeviceMissingDeviceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credential":"topsecret"}`))
	}))
	defer server.Close()
	if _, _, err := pairDevice(server.URL, "code", ""); err == nil || !strings.Contains(err.Error(), "device_id") {
		t.Fatalf("expected device_id error, got %v", err)
	}
}

func TestPairDeviceHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid or expired pairing code"}`))
	}))
	defer server.Close()
	if _, _, err := pairDevice(server.URL, "bad", ""); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("expected pairing error, got %v", err)
	}
}

func TestResolveDaemonIdentityPairBindsDeviceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"device-bound","credential":"secret-123"}`))
	}))
	defer server.Close()
	t.Setenv("AGORA_SERVER_URL", server.URL)
	t.Setenv("AGORA_DEVICE_NAME", "workstation")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	credentialPath := filepath.Join(dir, "device.credential")

	daemonID, credential, err := resolveDaemonIdentity("paircode", configPath, credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if daemonID != "device-bound" {
		t.Fatalf("daemon id = %q, want device-bound", daemonID)
	}
	if credential != "secret-123" {
		t.Fatalf("credential = %q, want secret-123", credential)
	}

	// The identity and credential must be persisted for later runs.
	reloadedID, reloadedCred, err := resolveDaemonIdentity("", configPath, credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedID != "device-bound" {
		t.Fatalf("later run resolved daemon id = %q, want device-bound", reloadedID)
	}
	if reloadedCred != "secret-123" {
		t.Fatalf("later run resolved credential = %q, want secret-123", reloadedCred)
	}
}

func TestResolveDaemonIdentityPairRejectsExplicitID(t *testing.T) {
	t.Setenv("AGORA_DAEMON_ID", "explicit-daemon")
	if _, _, err := resolveDaemonIdentity("paircode", filepath.Join(t.TempDir(), "c.json"), filepath.Join(t.TempDir(), "d.cred")); err == nil {
		t.Fatal("expected AGORA_DAEMON_ID + --pair conflict error")
	}
}

func TestResolveDaemonIdentityWithoutPairUsesStoredID(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	daemonID, credential, err := resolveDaemonIdentity("", configPath, filepath.Join(dir, "device.credential"))
	if err != nil {
		t.Fatal(err)
	}
	if daemonID == "" {
		t.Fatal("expected a daemon id")
	}
	// A second call resolves the same stored id, not a fresh one.
	again, _, err := resolveDaemonIdentity("", configPath, filepath.Join(dir, "device.credential"))
	if err != nil {
		t.Fatal(err)
	}
	if again != daemonID {
		t.Fatalf("daemon id changed between runs: %q -> %q", daemonID, again)
	}
	if credential != "" {
		t.Fatalf("credential without pairing = %q, want empty", credential)
	}
}
