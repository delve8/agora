package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/store"
)

func newDownloadTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
}

func TestInstallScriptBakesServerAndBaseURL(t *testing.T) {
	t.Setenv("AGORA_PUBLIC_URL", "https://agora.example.com/")
	srv := newDownloadTestServer(t)

	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/download/install.sh", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("install.sh returned %d", resp.Code)
	}
	if contentType := resp.Header().Get("Content-Type"); !strings.Contains(contentType, "shellscript") {
		t.Fatalf("install.sh content type = %q", contentType)
	}
	body := resp.Body.String()
	for _, want := range []string{
		`SERVER_URL="https://agora.example.com"`,
		`BASE_URL="https://agora.example.com"`,
		"agora-daemon.service",
		"LaunchAgents",
		`pair "$PAIR_CODE"`,
		"wget -qO",
		"EnvironmentVariables",
		"AGORA_PI_BINARY",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("install.sh is missing %q:\n%s", want, body)
		}
	}
}

func TestInstallScriptFallsBackToRequestHost(t *testing.T) {
	t.Setenv("AGORA_PUBLIC_URL", "")
	srv := newDownloadTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/download/install.sh", nil)
	req.Host = "agora.internal:8080"
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("install.sh returned %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), `SERVER_URL="http://agora.internal:8080"`) {
		t.Fatalf("install.sh did not fall back to the request host:\n%s", resp.Body.String())
	}
}

func TestDownloadServesAllowlistedArtifactsOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agora-linux-amd64"), []byte("linux-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGORA_DOWNLOAD_DIR", dir)
	srv := newDownloadTestServer(t)

	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/download/agora-linux-amd64", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "linux-binary" {
		t.Fatalf("artifact download = %d %q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Disposition"); !strings.Contains(got, "agora-linux-amd64") {
		t.Fatalf("content disposition = %q", got)
	}

	for _, path := range []string{"/download/secret.txt", "/download/..%2f..%2fetc%2fpasswd", "/download/checksums.txt"} {
		resp := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", path, resp.Code)
		}
	}
}

func TestDownloadRequiresConfiguredDirectory(t *testing.T) {
	t.Setenv("AGORA_DOWNLOAD_DIR", "")
	srv := newDownloadTestServer(t)

	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/download/agora-linux-amd64", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("download without AGORA_DOWNLOAD_DIR returned %d, want 404", resp.Code)
	}
}
