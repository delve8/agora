package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/config"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/store"
)

func readBuildInfo(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("/api/version returned %d: %s", resp.Code, resp.Body.String())
	}
	var value map[string]string
	if err := json.Unmarshal(resp.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// /api/version is what a workstation asks before updating: it needs the Server's
// own build and the version of the artifacts this Server publishes, because that
// is what would be installed.
func TestVersionEndpointReportsServerAndArtifactBuilds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("1.4.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGORA_DOWNLOAD_DIR", dir)
	srv := newDownloadTestServer(t)
	srv.SetBuildInfo("1.4.1", "abc1234", "2026-01-01T00:00:00Z")

	info := readBuildInfo(t, srv)
	if info["version"] != "1.4.1" || info["commit"] != "abc1234" || info["date"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("/api/version = %v, want the Server build", info)
	}
	if info["artifact_version"] != "1.4.0" {
		t.Fatalf("artifact_version = %q, want 1.4.0", info["artifact_version"])
	}
}

// A build without injected metadata must still answer something useful, and a
// deployment without a download directory must not fail.
func TestVersionEndpointWithoutBuildInfo(t *testing.T) {
	t.Setenv("AGORA_DOWNLOAD_DIR", "")
	srv := newDownloadTestServer(t)
	info := readBuildInfo(t, srv)
	if info["version"] != "unknown" {
		t.Fatalf("/api/version = %v, want an unknown version", info)
	}
	if _, ok := info["artifact_version"]; ok {
		t.Fatalf("/api/version = %v, want no artifact version without a download dir", info)
	}
}

// Health checks are the first thing a probe and an operator read; the version
// belongs there so a rolling deployment can be followed.
func TestHealthReportsVersion(t *testing.T) {
	srv := newDownloadTestServer(t)
	srv.SetBuildInfo("1.4.1", "abc1234", "2026-01-01T00:00:00Z")
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("/healthz returned %d", resp.Code)
	}
	var value map[string]string
	if err := json.Unmarshal(resp.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["status"] != "ok" || value["version"] != "1.4.1" {
		t.Fatalf("/healthz = %v", value)
	}
}

// The version a workstation compares against is reachable before signing in:
// updating must not require a browser session.
func TestVersionEndpointIsPublic(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := NewWithWebDirAndAuth(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil), t.TempDir(), config.ServerAuthConfig{Mode: config.AuthModeLogto, Issuer: "https://logto.example.com/oidc", Audience: "https://agora.example.com/api"})
	srv.SetBuildInfo("1.4.1", "abc1234", "2026-01-01T00:00:00Z")
	public := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if public.Code != http.StatusOK {
		t.Fatalf("/api/version in logto mode returned %d: %s", public.Code, public.Body.String())
	}
	// A protected endpoint still refuses the same request, so the allowlist is
	// not accidentally wide open.
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if resp.Code == http.StatusOK {
		t.Fatal("an unauthenticated /api/state was accepted in logto mode")
	}
}

// version.txt is served from the download directory like the binaries, so a
// workstation can learn the published version over the same origin.
func TestDownloadServesVersionTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("1.4.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGORA_DOWNLOAD_DIR", dir)
	srv := newDownloadTestServer(t)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/download/version.txt", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "1.4.0\n" {
		t.Fatalf("version.txt = %d %q", resp.Code, resp.Body.String())
	}
}
