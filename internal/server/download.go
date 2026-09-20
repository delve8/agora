package server

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed install.sh
var installScriptSource string

var installScriptTemplate = template.Must(template.New("install.sh").Parse(installScriptSource))

// downloadArtifacts lists what the Server publishes for `agora daemon` installs
// and doubles as an allowlist: only these names are ever served.
var downloadArtifacts = map[string]string{
	"agora-linux-amd64":  "application/octet-stream",
	"agora-linux-arm64":  "application/octet-stream",
	"agora-darwin-amd64": "application/octet-stream",
	"agora-darwin-arm64": "application/octet-stream",
	"checksums.txt":      "text/plain; charset=utf-8",
}

// installScript renders the daemon installer with this deployment's URLs baked
// in, so the command shown in the Web UI needs no extra arguments.
func (s *Server) installScript(w http.ResponseWriter, r *http.Request) {
	serverURL := s.publicBaseURL(r)
	baseURL := strings.TrimRight(strings.TrimSpace(s.publicURL), "/")
	if baseURL == "" {
		baseURL = serverURL
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := installScriptTemplate.Execute(w, map[string]string{"ServerURL": serverURL, "BaseURL": baseURL}); err != nil {
		http.Error(w, "failed to render install script", http.StatusInternalServerError)
	}
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	contentType, ok := downloadArtifacts[name]
	if !ok || strings.TrimSpace(s.downloadDir) == "" {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(filepath.Join(s.downloadDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// publicBaseURL builds an absolute base URL for the current request. It is the
// fallback when AGORA_PUBLIC_URL is unset, which is the local-development case.
func (s *Server) publicBaseURL(r *http.Request) string {
	if value := strings.TrimRight(strings.TrimSpace(s.publicURL), "/"); value != "" {
		return value
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return scheme + "://" + r.Host
}
