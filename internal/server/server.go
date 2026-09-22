package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/auth"
	"github.com/delve8/agora/internal/config"
	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/notification"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

type Server struct {
	HTTP    *http.Server
	store   *store.Store
	manager *runtime.Manager
	daemons *daemonHub
	auth    *auth.Authenticator
	// logtoClient is what the Web UI needs to start a sign-in. It is served
	// unauthenticated from /api/config so the bundle does not have to be built
	// per tenant.
	logtoClient config.LogtoClientConfig
	// publicURL is the externally reachable Server URL (AGORA_PUBLIC_URL). It is
	// baked into the daemon install script; when empty the request host is used.
	publicURL string
	// downloadDir holds the prebuilt daemon binaries published under /download.
	downloadDir string
	// buildInfo is what this Server is (and, through /api/version, what it
	// publishes): `agora update --check` compares it with the local binary.
	buildMu   sync.Mutex
	buildInfo map[string]string

	notifyMu       sync.Mutex
	notifyPolicies map[string]*notification.Policy
}

// SetBuildInfo records the version, commit and build date of this binary. It is
// set by the command that constructs the Server, which is where the linker's
// values live.
func (s *Server) SetBuildInfo(version, commit, date string) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	s.buildInfo = map[string]string{"version": version, "commit": commit, "date": date}
}

// buildInfoJSON is what /api/version and /healthz answer with. It also reports
// the version of the artifacts in the download directory, because that, not the
// Server's own build, is what a workstation installs: they are built together,
// but a half-updated deployment is worth seeing.
func (s *Server) buildInfoJSON() map[string]string {
	s.buildMu.Lock()
	info := make(map[string]string, len(s.buildInfo)+4)
	for key, value := range s.buildInfo {
		info[key] = value
	}
	s.buildMu.Unlock()
	if _, ok := info["version"]; !ok {
		info["version"] = "unknown"
	}
	if s.downloadDir != "" {
		if body, err := os.ReadFile(filepath.Join(s.downloadDir, "version.txt")); err == nil {
			if value := strings.TrimSpace(string(body)); value != "" {
				info["artifact_version"] = value
			}
		}
	}
	return info
}

func New(addr string, db *store.Store, manager *runtime.Manager) *Server {
	return NewWithWebDirAndAuth(addr, db, manager, "", config.ServerAuthConfig{Mode: config.AuthModeLocal})
}

func NewWithWebDir(addr string, db *store.Store, manager *runtime.Manager, webDir string) *Server {
	return NewWithWebDirAndAuth(addr, db, manager, webDir, config.ServerAuthConfig{Mode: config.AuthModeLocal})
}

func NewWithWebDirAndAuth(addr string, db *store.Store, manager *runtime.Manager, webDir string, authConfig config.ServerAuthConfig) *Server {
	if strings.TrimSpace(webDir) == "" {
		webDir = filepath.Join("web", "dist")
	}
	if authConfig.Mode == "" {
		authConfig.Mode = config.AuthModeLocal
	}
	var validator auth.TokenValidator
	if authConfig.Mode == config.AuthModeLogto {
		validator, _ = auth.NewLogtoTokenValidator(auth.LogtoValidatorOptions{Issuer: authConfig.Issuer, Audience: authConfig.Audience})
	}
	local := auth.Principal{UserID: "local", Provider: auth.ProviderLocal, DisplayName: "Local"}
	if db != nil {
		if principal, err := db.GetOrCreateLocalUser(context.Background()); err == nil {
			local = principal
		}
	}
	s := &Server{
		store: db, manager: manager, daemons: newDaemonHub(db, authConfig.Mode),
		auth:           auth.NewAuthenticatorWithProvisioning(authConfig.Mode, validator, db, local, authConfig.Provisioning),
		logtoClient:    config.ResolveLogtoClientConfig(authConfig.Issuer, os.Getenv("AGORA_LOGTO_ENDPOINT"), os.Getenv("AGORA_LOGTO_APP_ID"), authConfig.Audience),
		publicURL:      os.Getenv("AGORA_PUBLIC_URL"),
		downloadDir:    os.Getenv("AGORA_DOWNLOAD_DIR"),
		notifyPolicies: make(map[string]*notification.Policy),
	}
	s.daemons.onEvents = s.notifyObservedEvents
	s.daemons.onSessionAttention = s.notifySessionAttention
	s.daemons.onSessionExit = func(id string, code int, lastError string, _ bool) {
		s.notifyTaskResult(id, false, lastError, "")
	}
	if manager != nil {
		manager.SetEventHandler(func(item event.Event) { s.notifyObservedEvents(item.SessionID, []event.Event{item}) })
		manager.SetSessionExitHandler(func(value session.Session, exited runtime.AgentExit) {
			if exited.ExitCode != 0 && !exited.Intentional {
				s.notifyTaskResult(value.ID, false, value.LastError, "")
			}
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	// Daemon installer and prebuilt binaries. Public like /healthz: the pairing
	// code, not the download, is the secret.
	mux.HandleFunc("GET /download/install.sh", s.installScript)
	mux.HandleFunc("GET /download/{name}", s.downloadArtifact)
	// Public client configuration: the browser needs it before it can log in.
	mux.HandleFunc("GET /api/config", s.publicConfig)
	mux.HandleFunc("GET /api/version", s.version)
	mux.HandleFunc("GET /api/daemon/ws", s.daemons.serveHTTP)
	mux.HandleFunc("POST /api/daemon/pair", s.pairDaemon)
	mux.HandleFunc("POST /api/daemon/wrap", s.daemonWrap)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/devices/pair-codes", s.createPairCode)
	mux.HandleFunc("GET /api/devices", s.listDevices)
	mux.HandleFunc("POST /api/devices/{id}/revoke", s.revokeDevice)
	mux.HandleFunc("POST /api/devices/{id}/name", s.renameDevice)
	mux.HandleFunc("GET /api/webhooks", s.listWebhooks)
	mux.HandleFunc("POST /api/webhooks", s.createWebhook)
	mux.HandleFunc("PATCH /api/webhooks/{id}", s.updateWebhook)
	mux.HandleFunc("DELETE /api/webhooks/{id}", s.deleteWebhook)
	mux.HandleFunc("POST /api/webhooks/{id}/test", s.testWebhook)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/coordinations", s.createCoordination)
	mux.HandleFunc("GET /api/coordinations/{id}", s.getCoordination)
	mux.HandleFunc("POST /api/coordinations/{id}/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.updateSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.resumeSession)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.stopSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.getEvents)
	mux.HandleFunc("GET /api/sessions/{id}/events/stream", s.streamEvents)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.createMessage)
	mux.HandleFunc("GET /api/sessions/{id}/attach", s.attachAddr)
	mux.HandleFunc("GET /api/sessions/{id}/pty/snapshot", s.ptySnapshot)
	mux.Handle("/", s.frontendHandler(webDir))
	s.HTTP = &http.Server{Addr: addr, Handler: s.authMiddleware(mux)}
	return s
}

func (s *Server) frontendHandler(webDir string) http.Handler {
	if strings.TrimSpace(webDir) == "" {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.Dir(webDir))
	indexPath := filepath.Join(webDir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			if _, err := os.Stat(indexPath); err != nil {
				http.NotFound(w, r)
				return
			}
			// index.html points at hashed assets and must not be cached across a
			// rebuild; otherwise an old auth-less bundle can keep sending empty
			// Bearer credentials to a Logto-protected Server.
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, indexPath)
			return
		}
		relative := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(webDir, relative)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat(indexPath); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, indexPath)
	})
}

func (s *Server) Addr() string                       { return s.HTTP.Addr }
func (s *Server) Handler() http.Handler              { return s.HTTP.Handler }
func (s *Server) ListenAndServe() error              { return s.HTTP.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.HTTP.Shutdown(ctx) }

type sessionLocation int

const (
	sessionLocationStored sessionLocation = iota
	sessionLocationLocalHistory
	sessionLocationDaemonHistory
)

func (s *Server) effectiveStoredSession(ctx context.Context, value session.Session) (session.Session, error) {
	if value.Source == session.SourceManaged && s.manager != nil && s.manager.CanManageSessions() {
		effective := s.manager.EffectiveSession(value)
		if effective.State != value.State || effective.Connection != value.Connection || effective.ProcessID != value.ProcessID {
			if err := s.store.UpdateSessionObservation(ctx, effective); err != nil {
				return session.Session{}, err
			}
		}
		return effective, nil
	}
	if value.Source == session.SourceManaged {
		return s.daemons.effectiveSession(value), nil
	}
	if value.Source == session.SourceExternal {
		if value.ProcessID > 0 && adapter.ProcessAlive(value.ProcessID) {
			return value, nil
		}
		value.State = session.StateStopped
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || value.ClaudeSessionID != "", CanResume: value.ClaudeSessionID != "" && value.Workspace != "" && s.manager != nil && s.manager.CanManageSessions()}
		if err := s.store.UpdateSessionObservation(ctx, value); err != nil {
			return session.Session{}, err
		}
	}
	return value, nil
}

func (s *Server) resolveSession(ctx context.Context, id string) (session.Session, sessionLocation, error) {
	value, err := s.store.GetSession(ctx, id)
	if err == nil {
		if err := s.authorizeSession(ctx, value); err != nil {
			if errors.Is(err, errForbidden) {
				return session.Session{}, sessionLocationStored, err
			}
			return session.Session{}, sessionLocationStored, err
		}
		value, err = s.effectiveStoredSession(ctx, value)
		return value, sessionLocationStored, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, sessionLocationStored, err
	}
	coordinations, coordErr := s.store.ListCoordinations(ctx)
	if coordErr != nil {
		return session.Session{}, sessionLocationStored, coordErr
	}
	coordinationID := ""
	if len(coordinations) > 0 {
		coordinationID = coordinations[0].ID
	}
	if s.manager != nil && s.manager.CanManageSessions() {
		if value, ok, historyErr := s.manager.HistorySession(ctx, id, coordinationID, "local"); historyErr != nil {
			return session.Session{}, sessionLocationLocalHistory, historyErr
		} else if ok {
			if err := s.authorizeSession(ctx, value); err != nil {
				return session.Session{}, sessionLocationLocalHistory, err
			}
			return value, sessionLocationLocalHistory, nil
		}
	}
	if identity, identityErr := session.ParseSessionID(id); identityErr == nil {
		value := session.Session{ID: id, CoordinationID: coordinationID, DaemonID: identity.DaemonID, Agent: identity.Agent, AgentSessionID: identity.AgentSessionID, DisplayName: friendlySessionName(identity.Agent, ""), Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionObserved, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: true}}
		if err := s.authorizeSession(ctx, value); err != nil {
			return session.Session{}, sessionLocationDaemonHistory, err
		}
		if s.daemons.hasRoute(id) {
			return value, sessionLocationDaemonHistory, nil
		}
		value.Connection = session.ConnectionUnavailable
		return value, sessionLocationDaemonHistory, nil
	}
	return session.Session{}, sessionLocationStored, sql.ErrNoRows
}

var errForbidden = errors.New("forbidden")

func (s *Server) notifySessionAttention(sessionID, attention string) {
	if attention != string(notification.AttentionApprovalRequired) {
		return
	}
	value, err := s.resolveNotificationSession(context.Background(), sessionID)
	if err != nil {
		return
	}
	value.State = session.StateWaiting
	s.dispatchNotification(value, notification.AttentionApprovalRequired, "Agora · 需要用户介入", "Agent 正在原生终端等待你的确认，请打开终端处理后继续任务。", "")
}

func (s *Server) notifyTaskResult(sessionID string, success bool, summary, eventID string) {
	value, err := s.resolveNotificationSession(context.Background(), sessionID)
	if err != nil {
		return
	}
	attention := notification.AttentionCompleted
	title := "Agora · Agent 任务已完成"
	if !success {
		attention = notification.AttentionFailed
		title = "Agora · Agent 任务失败"
		value.State = session.StateFailed
	} else {
		value.State = "completed"
	}
	if strings.TrimSpace(summary) == "" {
		summary = "任务结果已产生，请打开 Agora 查看详情。"
	}
	s.dispatchNotification(value, attention, title, summary, eventID)
}

func (s *Server) notifyObservedEvents(sessionID string, values []event.Event) {
	if s.store == nil || len(values) == 0 {
		return
	}
	for _, item := range values {
		if !isTaskResultEvent(item) {
			continue
		}
		summary := item.Summary
		if summary == "" {
			summary = item.Content
		}
		s.notifyTaskResult(sessionID, !item.IsError, summary, item.ID)
	}
}

func isTaskResultEvent(item event.Event) bool {
	if item.Kind != event.KindResult && item.Kind != event.KindError {
		return false
	}
	// Proxy integrations may provide an already-normalized result without the
	// provider's raw record. A KindResult is sufficient in that case.
	if strings.TrimSpace(item.RawJSON) == "" {
		return item.Kind == event.KindResult
	}
	// Inspect the provider record rather than only the normalized Kind. Pi's
	// history begins with a `session` metadata record, which is not a task end.
	var raw struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(item.RawJSON), &raw) != nil {
		return false
	}
	switch raw.Type {
	case "result", "agent_end", "agent_settled", "turn_end":
		return true
	default:
		return false
	}
}

func (s *Server) resolveNotificationSession(ctx context.Context, id string) (session.Session, error) {
	value, err := s.store.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if value.Source == session.SourceManaged && s.manager == nil {
		value = s.daemons.effectiveSession(value)
	}
	return value, nil
}

func (s *Server) dispatchNotification(value session.Session, attention notification.Attention, title, summary, eventID string) {
	if s.store == nil {
		return
	}
	owner := "local"
	if value.DaemonID != "" {
		if userID, err := s.store.DeviceOwner(context.Background(), value.DaemonID); err == nil {
			owner = userID
		}
	}
	targets, err := s.store.ListWebhookTargets(context.Background(), owner)
	if err != nil || len(targets) == 0 {
		return
	}
	converted := make([]notification.Target, 0, len(targets))
	for _, target := range targets {
		converted = append(converted, notification.Target{ID: target.ID, Provider: notification.Provider(target.Provider), Label: target.Label, URL: target.URL, Enabled: target.Enabled})
	}
	s.notifyMu.Lock()
	policy := s.notifyPolicies[owner]
	if policy == nil {
		policy = notification.NewPolicy(30 * time.Second)
		s.notifyPolicies[owner] = policy
	}
	allowed := policy.Allow(notification.SessionNotification{ID: eventID, SessionID: value.ID, State: value.State, Attention: attention})
	s.notifyMu.Unlock()
	if !allowed {
		return
	}
	notifier := &notification.WebhookNotifier{Targets: converted, Client: &http.Client{Timeout: 10 * time.Second}}
	notificationID := eventID
	if notificationID == "" {
		notificationID = newID("notification")
	}
	go func() {
		_ = notifier.NotifySessionEvent(context.Background(), notification.SessionNotification{ID: notificationID, CoordinationID: value.CoordinationID, SessionID: value.ID, SessionName: value.DisplayName, State: value.State, Attention: attention, Title: title, Summary: summary, OpenURL: s.sessionOpenURL(value.ID), CreatedAt: time.Now().UTC()})
	}()
}

func (s *Server) sessionOpenURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("AGORA_PUBLIC_URL")), "/")
	if base == "" {
		return ""
	}
	return base + "/?session=" + url.QueryEscape(id)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	body := s.buildInfoJSON()
	body["status"] = "ok"
	writeJSON(w, http.StatusOK, body)
}

// version answers what this Server runs and what it publishes for workstations.
func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.buildInfoJSON())
}

// publicConfig tells the Web UI how to authenticate. It is intentionally
// unauthenticated (a client needs it before it can sign in) and exposes only
// public values: the Logto endpoint, the SPA application id and the API
// audience. Serving them from the Server is what lets one built bundle run
// against any tenant.
func (s *Server) publicConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_mode":      s.auth.Mode(),
		"logto_endpoint": s.logtoClient.Endpoint,
		"logto_app_id":   s.logtoClient.AppID,
		"logto_audience": s.logtoClient.Audience,
	})
}

// wrapperRequestInput is the body of POST /api/daemon/wrap.
type wrapperRequestInput struct {
	SessionID   string            `json:"session_id"`
	AgentArgs   []string          `json:"agent_args"`
	Workspace   string            `json:"workspace"`
	DisplayName string            `json:"display_name"`
	Role        string            `json:"role"`
	Agent       string            `json:"agent"`
	Prompts     []string          `json:"prompts"`
	Terminal    map[string]string `json:"terminal"`
}

// wrapperCreatePayload is the Daemon frame a wrapper request turns into. It is a
// separate function so a new field in the wrapper protocol cannot be added here
// and silently forgotten on the way to the Daemon.
func wrapperCreatePayload(input wrapperRequestInput, daemonID string) protocol.SessionCreatePayload {
	return protocol.SessionCreatePayload{
		AgentArgs: input.AgentArgs,
		Role:      input.Role,
		Terminal:  input.Terminal,
		DaemonID:  daemonID,
	}
}

// daemonWrap is the local terminal wrapper API. It authenticates with the
// Daemon's device credential, not a Web user's Logto token. This keeps the
// wrapper independent from browser login while preserving device ownership.
func (s *Server) daemonWrap(w http.ResponseWriter, r *http.Request) {
	provided := auth.ExtractBearer(r.Header.Get("Authorization"))
	daemonID := strings.TrimSpace(r.Header.Get("X-Agora-Daemon-ID"))
	if daemonID == "" {
		writeErrorStatus(w, http.StatusUnauthorized, errors.New("daemon id is required"))
		return
	}
	if s.auth.Mode() == config.AuthModeLogto {
		if provided == "" || s.store == nil {
			writeErrorStatus(w, http.StatusUnauthorized, errors.New("daemon authentication required"))
			return
		}
		device, err := s.store.GetDeviceByCredentialHash(r.Context(), store.HashSecret(provided))
		if err != nil || device.RevokedAt != nil || device.ID != daemonID {
			writeErrorStatus(w, http.StatusUnauthorized, errors.New("daemon authentication required"))
			return
		}
	} else if s.daemons == nil || !s.daemons.isConnected(daemonID) {
		// Trust-local mode has no device credential. The Unix socket is
		// permission-protected and the daemon's live WebSocket registration is
		// the server-side proof that this daemon is currently available.
		writeErrorStatus(w, http.StatusUnauthorized, errors.New("daemon is not connected"))
		return
	}
	if s.daemons == nil || s.store == nil || !s.daemons.isConnected(daemonID) {
		writeErrorStatus(w, http.StatusServiceUnavailable, fmt.Errorf("daemon %s is offline or unavailable", daemonID))
		return
	}
	var input wrapperRequestInput
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if input.SessionID != "" {
		identity, err := session.ParseSessionID(input.SessionID)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, err)
			return
		}
		if identity.DaemonID != daemonID {
			writeErrorStatus(w, http.StatusForbidden, errors.New("session is not owned by this daemon"))
			return
		}
		if identity.Agent != "claude" && identity.Agent != "pi" {
			writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("unsupported agent %q", identity.Agent))
			return
		}
		frame, err := s.daemons.request(r.Context(), input.SessionID, protocol.AttachRequest, protocol.AttachPayload{SessionID: input.SessionID}, protocol.AttachResponse)
		if err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		var attached protocol.AttachPayload
		if err := protocol.DecodePayload(frame, &attached); err != nil || attached.Socket == "" {
			if err == nil && attached.Error != "" {
				err = errors.New(attached.Error)
			}
			if err == nil {
				err = errors.New("daemon returned no attach socket")
			}
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": input.SessionID, "socket": attached.Socket})
		return
	}
	agent := strings.ToLower(strings.TrimSpace(input.Agent))
	if agent == "" {
		agent = "claude"
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = "New session"
	}
	if agent == "claude-code" {
		agent = "claude"
	}
	if agent != "claude" && agent != "pi" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("unsupported agent %q", agent))
		return
	}
	coordinationID := ""
	coordinations, err := s.store.ListCoordinations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if len(coordinations) == 0 {
		coord := coordination.Coordination{ID: newID("coord"), Name: "Default Coordination", CreatedAt: time.Now().UTC()}
		if err := s.store.CreateCoordination(r.Context(), coord); err != nil {
			writeError(w, err)
			return
		}
		coordinationID = coord.ID
	} else {
		coordinationID = coordinations[0].ID
	}
	rawWorkspace := strings.TrimSpace(input.Workspace)
	if rawWorkspace == "" {
		writeErrorStatus(w, http.StatusBadRequest, errors.New("workspace is required"))
		return
	}
	workspace, err := filepath.Abs(rawWorkspace)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}

	// The workspace belongs to the Daemon host, not necessarily the Server
	// host. The Daemon validates that this absolute path exists locally when it
	// handles the session.create frame.
	payload := wrapperCreatePayload(input, daemonID)
	payload.CoordinationID = coordinationID
	payload.Workspace = workspace
	payload.DisplayName = displayName
	payload.Agent = agent
	result, err := s.daemons.createSession(r.Context(), payload, daemonID)
	if err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	identity, err := session.ParseSessionID(result.SessionID)
	if err != nil || identity.DaemonID != daemonID || (identity.Agent != "claude" && identity.Agent != "pi") {
		if err == nil {
			err = fmt.Errorf("daemon returned session %q for daemon %q", result.SessionID, daemonID)
		}
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	if result.Agent != "" && result.Agent != identity.Agent {
		writeErrorStatus(w, http.StatusBadGateway, fmt.Errorf("daemon returned agent %q for session agent %q", result.Agent, identity.Agent))
		return
	}
	if result.AgentSessionID == "" {
		result.AgentSessionID = identity.AgentSessionID
	}
	storedWorkspace := workspace
	if strings.TrimSpace(result.Workspace) != "" {
		storedWorkspace = result.Workspace
	}
	value := session.Session{ID: result.SessionID, CoordinationID: coordinationID, DaemonID: daemonID, Agent: result.Agent, AgentSessionID: result.AgentSessionID, HistoryPath: result.HistoryPath, Workspace: storedWorkspace, DisplayName: displayName, Role: input.Role, State: session.StateRunning, Source: session.SourceManaged, Connection: session.ConnectionObserved, ProcessID: result.PID, Capabilities: capabilitiesFromMap(result.Capabilities), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if value.Agent == "" {
		value.Agent = agent
	}
	if err := s.store.CreateSession(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	s.daemons.upsertLiveSession(value)
	frame, err := s.daemons.request(r.Context(), result.SessionID, protocol.AttachRequest, protocol.AttachPayload{SessionID: result.SessionID}, protocol.AttachResponse)
	if err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	var attached protocol.AttachPayload
	if err := protocol.DecodePayload(frame, &attached); err != nil || attached.Socket == "" {
		if err == nil && attached.Error != "" {
			err = errors.New(attached.Error)
		}
		if err == nil {
			err = errors.New("daemon returned no attach socket")
		}
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	// Initial prompts are sent over the daemon protocol after the PTY exists.
	// They are intentionally not sent through the user-authenticated Web API.
	for _, prompt := range input.Prompts {
		if strings.TrimSpace(prompt) == "" {
			continue
		}
		if err := s.daemons.sessionInput(r.Context(), value, prompt); err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"session_id": result.SessionID, "socket": attached.Socket})
}

func (s *Server) authorizeSession(ctx context.Context, value session.Session) error {
	principal, err := auth.RequirePrincipal(ctx)
	if err != nil {
		return err
	}
	if s.auth.Mode() == config.AuthModeLocal {
		return nil
	}
	daemonID := value.DaemonID
	if daemonID == "" {
		if identity, parseErr := value.Identity(); parseErr == nil {
			daemonID = identity.DaemonID
		}
	}
	if daemonID == "" {
		return errors.New("session has no daemon ownership")
	}
	owned, err := s.store.UserOwnsDaemon(ctx, principal.UserID, daemonID)
	if err != nil {
		return err
	}
	if !owned {
		return errForbidden
	}
	return nil
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/config" || r.URL.Path == "/api/version" || r.URL.Path == "/api/daemon/ws" || r.URL.Path == "/api/daemon/pair" || r.URL.Path == "/api/daemon/wrap" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && !sameOriginRequest(r) {
			writeErrorStatus(w, http.StatusForbidden, errors.New("cross-origin request is not allowed"))
			return
		}
		principal, err := s.auth.Authenticate(r.Context(), auth.ExtractBearer(r.Header.Get("Authorization")))
		if err != nil {
			writeErrorStatus(w, http.StatusUnauthorized, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
}

func sameOriginRequest(r *http.Request) bool {
	for _, header := range []string{"Origin", "Referer"} {
		value := strings.TrimSpace(r.Header.Get(header))
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || !sameHost(u.Host, r.Host) {
			return false
		}
	}
	return true
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": principal, "auth_mode": s.auth.Mode()})
}

func (s *Server) createPairCode(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeError(w, err)
		return
	}
	code := fmt.Sprintf("%x", raw[:])
	expires := time.Now().UTC().Add(10 * time.Minute)
	if err := s.store.CreatePairCode(r.Context(), principal.UserID, store.HashSecret(code), expires); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": expires})
}

func (s *Server) pairDaemon(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	code := strings.TrimSpace(input.Code)
	if code == "" {
		writeErrorStatus(w, http.StatusBadRequest, errors.New("code is required"))
		return
	}
	userID, err := s.store.ConsumePairCode(r.Context(), store.HashSecret(code), time.Now().UTC())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, errors.New("invalid or expired pairing code"))
		return
	}
	var credentialBytes [32]byte
	if _, err := rand.Read(credentialBytes[:]); err != nil {
		writeError(w, err)
		return
	}
	credential := fmt.Sprintf("%x", credentialBytes[:])
	deviceID := newID("device")
	if err := s.store.CreateDevice(r.Context(), store.Device{ID: deviceID, UserID: userID, CredentialHash: store.HashSecret(credential), Name: sanitizeDeviceName(input.Name), CreatedAt: time.Now().UTC()}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"device_id": deviceID, "credential": credential})
}

// deviceDTO is the wire contract for a paired device. It deliberately never
// carries the credential hash or internal user id, and includes whether the
// daemon is currently connected so the UI can show online state.
type deviceDTO struct {
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	Connected  bool       `json:"connected"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

const maxDeviceNameLength = 64

// sanitizeDeviceName trims and truncates a device display alias to a bounded
// length. Names are labels only and never used as the daemon identity.
func sanitizeDeviceName(name string) string {
	name = strings.TrimSpace(name)
	if limit := maxDeviceNameLength; len([]rune(name)) > limit {
		name = string([]rune(name)[:limit])
	}
	return name
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	values, err := s.store.ListDevices(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	devices := make([]deviceDTO, 0, len(values))
	for _, value := range values {
		devices = append(devices, deviceDTO{
			DeviceID:   value.ID,
			Name:       value.Name,
			Connected:  s.daemons.isConnected(value.ID),
			CreatedAt:  value.CreatedAt,
			LastSeenAt: value.LastSeenAt,
			RevokedAt:  value.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, devices)
}

type webhookDTO struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
}

func webhookResponse(value store.WebhookTarget) webhookDTO {
	return webhookDTO{ID: value.ID, Provider: value.Provider, Label: value.Label, URL: maskWebhookURL(value.URL), Enabled: value.Enabled}
}

func maskWebhookURL(raw string) string {
	value, err := url.Parse(raw)
	if err != nil {
		return "configured"
	}
	if value.User != nil {
		value.User = url.User("***")
	}
	if value.RawQuery != "" {
		value.RawQuery = "***"
	}
	if value.Fragment != "" {
		value.Fragment = "***"
	}
	return value.String()
}

func validateWebhookURL(raw string) (string, error) {
	value, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || value.Scheme != "http" && value.Scheme != "https" || value.Host == "" {
		return "", errors.New("webhook URL must be an http or https URL")
	}
	return value.String(), nil
}

func webhookProvider(value string) notification.Provider {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return notification.ProviderGeneric
	}
	return notification.Provider(value)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	values, err := s.store.ListWebhookTargets(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	result := make([]webhookDTO, 0, len(values))
	for _, value := range values {
		result = append(result, webhookResponse(value))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	var input struct {
		Provider string `json:"provider"`
		Label    string `json:"label"`
		URL      string `json:"url"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	u, err := validateWebhookURL(input.URL)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	provider := webhookProvider(input.Provider)
	if _, _, err := notification.Payload(provider, notification.SessionNotification{Title: "test"}); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().UTC()
	value := store.WebhookTarget{ID: newID("webhook"), UserID: principal.UserID, Provider: string(provider), Label: sanitizeDeviceName(input.Label), URL: u, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateWebhookTarget(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, webhookResponse(value))
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	id := r.PathValue("id")
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if input.Enabled == nil {
		writeErrorStatus(w, http.StatusBadRequest, errors.New("enabled is required"))
		return
	}
	if err := s.store.SetWebhookTargetEnabled(r.Context(), principal.UserID, id, *input.Enabled); errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	value, err := s.store.GetWebhookTarget(r.Context(), principal.UserID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookResponse(value))
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	err = s.store.DeleteWebhookTarget(r.Context(), principal.UserID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	value, err := s.store.GetWebhookTarget(r.Context(), principal.UserID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	notifier := &notification.WebhookNotifier{Client: &http.Client{Timeout: 10 * time.Second}, Targets: []notification.Target{{ID: value.ID, Provider: notification.Provider(value.Provider), Label: value.Label, URL: value.URL, Enabled: true}}}
	err = notifier.NotifySessionEvent(r.Context(), notification.SessionNotification{ID: newID("notification"), SessionName: "Webhook test", State: "test", Attention: notification.AttentionCompleted, Title: "Agora Webhook 测试", Summary: "Webhook 配置正常。", CreatedAt: time.Now().UTC()})
	if err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (s *Server) renameDevice(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	deviceID := r.PathValue("id")
	owned, err := s.store.UserOwnsDaemon(r.Context(), principal.UserID, deviceID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !owned {
		writeErrorStatus(w, http.StatusNotFound, sql.ErrNoRows)
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	name := sanitizeDeviceName(input.Name)
	if name == "" {
		writeErrorStatus(w, http.StatusBadRequest, errors.New("device name is required"))
		return
	}
	if err := s.store.UpdateDeviceName(r.Context(), deviceID, name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"renamed": true})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	deviceID := r.PathValue("id")
	owned, err := s.store.UserHasDevice(r.Context(), principal.UserID, deviceID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !owned {
		writeErrorStatus(w, http.StatusNotFound, sql.ErrNoRows)
		return
	}
	if err := s.store.RevokeDevice(r.Context(), deviceID, time.Now().UTC()); err != nil {
		writeError(w, err)
		return
	}
	// Revoke is effective immediately for an already-connected daemon. The
	// store flag rejects future logto connections; disconnectDaemon removes the
	// current connection's live sessions, history and routes right away.
	s.daemons.disconnectDaemon(deviceID)
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// liveSessions builds the session list from live sources only: connected
// daemons (daemon mode) or the local manager (serve mode). It never reads
// sessions back from the store, so stale persisted rows for offline daemons
// never surface.
func (s *Server) liveSessions(ctx context.Context, coordinationID string) ([]session.Session, error) {
	sessions := make([]session.Session, 0)
	if s.manager != nil && s.manager.CanManageSessions() {
		live, err := s.manager.LiveSessions(ctx, coordinationID)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, live...)
	} else {
		sessions = append(sessions, s.daemons.liveSessions(coordinationID)...)
	}
	return sessions, nil
}

// visibleSessions is the isolation boundary for every session-list response.
// A device has exactly one owner, so a user must only ever see sessions on the
// devices they own; the daemon hub holds every connected daemon's sessions, so
// without this filter another user's sessions would leak into the list even
// though acting on them is already forbidden. Revoked devices are hidden too.
// trust-local mode is the deployment's own single-user trust boundary and does
// not filter.
func (s *Server) visibleSessions(ctx context.Context, values []session.Session) ([]session.Session, error) {
	if s.store == nil || len(values) == 0 {
		return values, nil
	}
	principal, err := auth.RequirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if s.auth.Mode() == config.AuthModeLocal {
		return values, nil
	}
	devices, err := s.store.ListDevices(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(devices))
	for _, device := range devices {
		if device.RevokedAt == nil {
			owned[device.ID] = true
		}
	}
	filtered := values[:0]
	for _, value := range values {
		daemonID := value.DaemonID
		if daemonID == "" {
			if identity, parseErr := session.ParseSessionID(value.ID); parseErr == nil {
				daemonID = identity.DaemonID
			}
		}
		if !owned[daemonID] {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, nil
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	coordinations, err := s.store.ListCoordinations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if len(coordinations) == 0 {
		value := coordination.Coordination{ID: newID("coord"), Name: "Default Coordination", CreatedAt: time.Now().UTC()}
		if err := s.store.CreateCoordination(r.Context(), value); err != nil {
			writeError(w, err)
			return
		}
		coordinations = append(coordinations, value)
	}
	coord := coordinations[0]
	sessions, err := s.liveSessions(r.Context(), coord.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	sessions, err = s.visibleSessions(r.Context(), sessions)
	if err != nil {
		writeError(w, err)
		return
	}
	// Merge discovered history (manager catalog or connected daemons) into the
	// live list, deduplicating by the provider-specific agent session URI. Do
	// not use ClaudeSessionID here: it is empty for Pi, and using an empty map
	// key would merge every Pi history entry into the first Pi session.
	sessionKeys := make(map[string]int, len(sessions))
	for index, value := range sessions {
		for _, key := range sessionMergeKeys(value) {
			sessionKeys[key] = index
		}
	}
	mergeDiscovered := func(value session.Session) {
		keys := sessionMergeKeys(value)
		for _, key := range keys {
			if index, exists := sessionKeys[key]; exists {
				// A managed session can reach the Server before its observer has
				// resolved the native URI or history path. Keep the live entry as
				// the primary record, but enrich it with metadata from discovery.
				if value.AgentSessionID != "" && sessions[index].AgentSessionID == "" {
					sessions[index].AgentSessionID = value.AgentSessionID
				}

				// A managed wrapper starts with a generated name. Let provider
				// history replace it for every Agent; the workspace is already the
				// parent Cascader item, so it should not hide the session name.
				if shouldEnrichSessionName(sessions[index]) && strings.TrimSpace(value.DisplayName) != "" {
					sessions[index].DisplayName = value.DisplayName
					sessions[index].DisplayNameSource = value.DisplayNameSource
				}
				if sessions[index].HistoryPath == "" {
					sessions[index].HistoryPath = value.HistoryPath
				}
				if sessions[index].Workspace == "" {
					sessions[index].Workspace = value.Workspace
				}
				if value.UpdatedAt.After(sessions[index].UpdatedAt) {
					sessions[index].UpdatedAt = value.UpdatedAt
				}
				if !sessions[index].Capabilities.CanSendInput {
					sessions[index].Capabilities.CanReadHistory = true
					sessions[index].Capabilities.CanResume = value.Capabilities.CanResume
				}
				return
			}
		}
		// A live session can reach the Server before its observer resolved the
		// native URI and history path. Only that unresolved hand-off can be
		// completed from discovery: if there is exactly one live session for this
		// provider/workspace whose identity is still unknown, treat that row as
		// the same session.
		//
		// A live session that already knows its own native session is a different
		// conversation from any other history row in the same workspace, so
		// merging there would hide real sessions. Requiring a unique unresolved
		// candidate keeps both directions correct.
		if value.Source == session.SourceHistory && value.Agent != "" && value.Workspace != "" {
			candidate := -1
			for index, live := range sessions {
				if !isLiveSession(live) || live.Agent != value.Agent || filepath.Clean(live.Workspace) != filepath.Clean(value.Workspace) {
					continue
				}
				if strings.TrimSpace(live.AgentSessionID) != "" {
					continue
				}
				if live.HistoryPath != "" && value.HistoryPath != "" && filepath.Clean(live.HistoryPath) != filepath.Clean(value.HistoryPath) {
					continue
				}
				if candidate != -1 {
					candidate = -2
					break
				}
				candidate = index
			}
			if candidate >= 0 {
				sessionKeys["workspace\x00"+value.Agent+"\x00"+filepath.Clean(value.Workspace)] = candidate
				mergeDiscoveredIntoSession(&sessions, candidate, value)
				return
			}
		}
		sessions = append(sessions, value)
		for _, key := range keys {
			sessionKeys[key] = len(sessions) - 1
		}
	}
	if s.manager != nil && s.manager.CanManageSessions() {
		discovered, historyErr := s.manager.HistorySessions(r.Context(), coord.ID, "local")
		if historyErr != nil {
			writeError(w, historyErr)
			return
		}
		for _, value := range discovered {
			mergeDiscovered(value)
		}
	}
	for _, value := range s.daemons.historySessions(coord.ID) {
		mergeDiscovered(value)
	}
	sessions, err = s.visibleSessions(r.Context(), sessions)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.applySessionPreferences(r.Context(), sessions); err != nil {
		writeError(w, err)
		return
	}
	// A live session is actively working even when it has not yet written a new
	// JSONL record (thinking, waiting on a tool, etc.). Lift its sort timestamp
	// to "now" so recently-active conversations outrank stale-but-later history.
	now := time.Now().UTC()
	for index := range sessions {
		if isLiveSession(sessions[index]) {
			sessions[index].UpdatedAt = now
		}
	}
	// A starred session is the one the user currently cares about, so it sorts
	// ahead of every unstarred session regardless of recency.
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Starred != sessions[j].Starred {
			return sessions[i].Starred
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{"coordination": coord, "sessions": sessions})
}

// sessionPreferenceKey is where a per-user preference is stored. The provider
// native URI is preferred because it survives a canonical id change caused by a
// resume/rebind; the canonical id is the fallback while the native URI is still
// being resolved.
func sessionPreferenceKey(value session.Session) string {
	if uri := strings.TrimSpace(value.NativeSessionURI()); uri != "" {
		return uri
	}
	return strings.TrimSpace(value.ID)
}

func (s *Server) lookupSessionPreference(ctx context.Context, userID string, value session.Session) (store.SessionPreference, error) {
	keys := make([]string, 0, 2)
	if uri := strings.TrimSpace(value.NativeSessionURI()); uri != "" {
		keys = append(keys, uri)
	}
	if id := strings.TrimSpace(value.ID); id != "" && (len(keys) == 0 || keys[0] != id) {
		keys = append(keys, id)
	}
	for _, key := range keys {
		preference, err := s.store.GetSessionPreference(ctx, userID, key)
		if err == nil {
			return preference, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.SessionPreference{}, err
		}
	}
	return store.SessionPreference{}, sql.ErrNoRows
}

func (s *Server) saveSessionPreference(ctx context.Context, userID string, value session.Session, preference store.SessionPreference) error {
	preference.UserID = userID
	preference.SessionKey = sessionPreferenceKey(value)
	if preference.SessionKey == "" {
		return fmt.Errorf("session has no id")
	}
	if err := s.store.UpsertSessionPreference(ctx, preference); err != nil {
		return err
	}
	// Drop a stale row left under the canonical id before the native URI was
	// resolved, so the same session does not keep two preferences.
	if id := strings.TrimSpace(value.ID); id != "" && id != preference.SessionKey {
		_ = s.store.DeleteSessionPreference(ctx, userID, id)
	}
	return nil
}

// applySessionPreferences overlays the requesting user's alias and starred flag
// on the merged session list. It runs after discovery so a user alias always
// wins over a derived name, and before sorting so starred sessions lead.
func (s *Server) applySessionPreferences(ctx context.Context, sessions []session.Session) error {
	if s.store == nil || len(sessions) == 0 {
		return nil
	}
	principal, err := auth.RequirePrincipal(ctx)
	if err != nil {
		return err
	}
	preferences, err := s.store.ListSessionPreferences(ctx, principal.UserID)
	if err != nil {
		return err
	}
	if len(preferences) == 0 {
		return nil
	}
	byKey := make(map[string]store.SessionPreference, len(preferences))
	for _, preference := range preferences {
		byKey[preference.SessionKey] = preference
	}
	for index := range sessions {
		preference, ok := byKey[strings.TrimSpace(sessions[index].NativeSessionURI())]
		if !ok {
			preference, ok = byKey[strings.TrimSpace(sessions[index].ID)]
		}
		if !ok {
			continue
		}
		sessions[index].Starred = preference.Starred
		if name := strings.TrimSpace(preference.DisplayName); name != "" {
			sessions[index].DisplayName = name
			sessions[index].DisplayNameSource = session.DisplayNameSourceCustom
		}
	}
	return nil
}

func shouldEnrichSessionName(current session.Session) bool {
	name := strings.TrimSpace(current.DisplayName)
	if name == "" || name == "." || session.IsGeneratedDisplayName(name) || isOpaqueSessionName(name, current.Agent) {
		return true
	}
	// Older wrapper-created rows used the workspace basename as the child name.
	// This can already be marked custom by an older server, so compare the
	// actual legacy shape rather than relying only on DisplayNameSource.
	return current.Workspace != "" && filepath.Base(filepath.Clean(current.Workspace)) == name
}

func mergeDiscoveredIntoSession(sessions *[]session.Session, index int, value session.Session) {
	if index < 0 || index >= len(*sessions) {
		return
	}
	current := &(*sessions)[index]
	if value.AgentSessionID != "" && current.AgentSessionID == "" {
		current.AgentSessionID = value.AgentSessionID
	}
	if current.HistoryPath == "" {
		current.HistoryPath = value.HistoryPath
	}
	if current.Workspace == "" {
		current.Workspace = value.Workspace
	}
	if current.DisplayName == "" || current.DisplayName == "." || session.IsGeneratedDisplayName(current.DisplayName) || isOpaqueSessionName(current.DisplayName, current.Agent) {
		if value.DisplayName != "" {
			current.DisplayName = value.DisplayName
			current.DisplayNameSource = value.DisplayNameSource
		}
	}
	if value.UpdatedAt.After(current.UpdatedAt) {
		current.UpdatedAt = value.UpdatedAt
	}
	if !current.Capabilities.CanSendInput {
		current.Capabilities.CanReadHistory = true
		current.Capabilities.CanResume = value.Capabilities.CanResume
	}
}

func sessionMergeKey(value session.Session) string {
	keys := sessionMergeKeys(value)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func sessionMergeKeys(value session.Session) []string {
	keys := make([]string, 0, 2)
	if uri := strings.TrimSpace(value.NativeSessionURI()); uri != "" {
		// The provider is already part of the URI (claude://..., pi://...).
		keys = append(keys, "uri\x00"+uri)
	}
	// During the short interval between PTY creation and observer metadata
	// reconciliation, a managed session may have no native URI on the Server
	// while the catalog entry already has the same provider history path. Use
	// that path as a temporary identity fallback. It prevents one physical
	// provider session from being rendered as separate TUI and history rows;
	// the URI remains the authoritative key once available.
	if path := strings.TrimSpace(value.HistoryPath); path != "" {
		keys = append(keys, "path\x00"+value.Agent+"\x00"+filepath.Clean(path))
	}
	if len(keys) == 0 && value.ID != "" {
		keys = append(keys, "id\x00"+value.ID)
	}
	return keys
}

func isLiveSession(value session.Session) bool {
	if value.ProcessID <= 0 {
		return false
	}
	switch value.State {
	case session.StateRunning, session.StateWaiting, session.StateStarting:
		return true
	}
	return false
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	coordinationID := r.PathValue("id")
	if _, err := s.store.GetCoordination(r.Context(), coordinationID); err != nil {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	var input struct {
		Workspace   string `json:"workspace"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Agent       string `json:"agent"`
		DaemonID    string `json:"daemon_id"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	workspace := strings.TrimSpace(input.Workspace)
	if workspace == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("workspace is required"))
		return
	}
	daemonID := strings.TrimSpace(input.DaemonID)
	if daemonID != "" {
		if s.manager != nil && s.manager.CanManageSessions() {
			writeErrorStatus(w, http.StatusBadRequest, errors.New("device targeting is not available in serve mode"))
			return
		}
		if s.auth.Mode() == config.AuthModeLogto {
			principal, principalErr := auth.RequirePrincipal(r.Context())
			if principalErr != nil {
				writeErrorStatus(w, http.StatusUnauthorized, principalErr)
				return
			}
			owned, ownedErr := s.store.UserOwnsDaemon(r.Context(), principal.UserID, daemonID)
			if ownedErr != nil {
				writeError(w, ownedErr)
				return
			}
			if !owned {
				writeErrorStatus(w, http.StatusNotFound, sql.ErrNoRows)
				return
			}
		}
	}
	if s.manager != nil && s.manager.CanManageSessions() {
		var err error
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, err)
			return
		}
		info, err := os.Stat(workspace)
		if err != nil || !info.IsDir() {
			writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("workspace must be an existing directory"))
			return
		}
	}
	if s.manager != nil && s.manager.CanManageSessions() {
		agent := strings.ToLower(strings.TrimSpace(input.Agent))
		if agent == "" {
			agent = "claude-code"
		}
		if agent != "claude" && agent != "claude-code" && agent != "pi" {
			writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("unsupported agent %q", agent))
			return
		}
		value, err := s.manager.CreateManagedSessionWithAgent(r.Context(), newID("sess"), coordinationID, workspace, input.DisplayName, input.Role, agent)
		if err != nil {
			writeErrorStatus(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, value)
		return
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = "New session"
	}
	if s.daemons == nil {
		writeErrorStatus(w, http.StatusServiceUnavailable, fmt.Errorf("daemon service is unavailable"))
		return
	}
	result, err := s.daemons.createSession(r.Context(), protocol.SessionCreatePayload{CoordinationID: coordinationID, Workspace: workspace, DisplayName: displayName, Role: input.Role, Agent: input.Agent, DaemonID: daemonID}, daemonID)
	if err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	value := session.Session{ID: result.SessionID, CoordinationID: coordinationID, DaemonID: result.DaemonID, Agent: result.Agent, AgentSessionID: result.AgentSessionID, HistoryPath: result.HistoryPath, Workspace: workspace, DisplayName: displayName, DisplayNameSource: session.InitialDisplayNameSource(displayName), Role: input.Role, State: session.StateRunning, Source: session.SourceManaged, Connection: session.ConnectionObserved, ProcessID: result.PID, Capabilities: capabilitiesFromMap(result.Capabilities), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.store.CreateSession(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	s.daemons.upsertLiveSession(value)
	writeJSON(w, http.StatusCreated, value)
}

func capabilitiesFromMap(values map[string]bool) session.Capabilities {
	return session.Capabilities{CanStart: values["can_start"], CanDiscover: values["can_discover"], CanAttach: values["can_attach"], CanObserve: values["can_observe"], CanSendInput: values["can_send_input"], CanStream: values["can_stream"], CanInterrupt: values["can_interrupt"], CanResume: values["can_resume"], CanApprove: values["can_approve"], CanReadHistory: values["can_read_history"], CanReadTerminal: values["can_read_terminal"]}
}

func (s *Server) createCoordination(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = "Coordination"
	}
	value := coordination.Coordination{ID: newID("coord"), Name: name, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateCoordination(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (s *Server) getCoordination(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, err := s.store.GetCoordination(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	sessions, err := s.liveSessions(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	sessions, err = s.visibleSessions(r.Context(), sessions)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coordination": value, "sessions": sessions})
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, location, err := s.resolveSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !value.Capabilities.CanResume {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session cannot be resumed"))
		return
	}
	if location == sessionLocationDaemonHistory || s.manager == nil || !s.manager.CanManageSessions() {
		resumed, resumeErr := s.daemons.resumeSession(r.Context(), value)
		if resumeErr != nil {
			writeErrorStatus(w, http.StatusBadGateway, resumeErr)
			return
		}
		if _, getErr := s.store.GetSession(r.Context(), resumed.ID); errors.Is(getErr, sql.ErrNoRows) {
			if createErr := s.store.CreateSession(r.Context(), resumed); createErr != nil {
				writeError(w, createErr)
				return
			}
		} else if getErr == nil {
			_ = s.store.UpdateSessionObservation(r.Context(), resumed)
		} else {
			writeError(w, getErr)
			return
		}
		s.daemons.upsertLiveSession(resumed)
		writeJSON(w, http.StatusOK, resumed)
		return
	}
	resumed, err := s.manager.ResumeSession(r.Context(), value)
	if err != nil {
		writeErrorStatus(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, resumed)
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	value, _, err := s.resolveSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !value.Capabilities.CanInterrupt {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session cannot be stopped"))
		return
	}
	if s.manager != nil && s.manager.IsRunning(value.ID) {
		err = s.manager.StopSession(value)
	} else {
		err = s.daemons.stopSession(r.Context(), value)
	}
	if err != nil {
		writeErrorStatus(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	value, _, err := s.resolveSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

type sessionPatchPayload struct {
	Starred     *bool   `json:"starred"`
	DisplayName *string `json:"display_name"`
}

// updateSession sets the requesting user's alias and/or starred flag for a
// session. These are Server-side preferences, not provider state, so they work
// for live managed sessions and discovered history alike.
func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	value, _, err := s.resolveSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var payload sessionPatchPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if payload.Starred == nil && payload.DisplayName == nil {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("starred or display_name is required"))
		return
	}
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	preference, err := s.lookupSessionPreference(r.Context(), principal.UserID, value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	if payload.Starred != nil {
		preference.Starred = *payload.Starred
	}
	if payload.DisplayName != nil {
		preference.DisplayName = session.DescribeMessage(strings.TrimSpace(*payload.DisplayName))
	}
	if preference.DisplayName == "" && !preference.Starred {
		// The row may live under either key depending on when the native URI was
		// resolved, so clear both.
		for _, key := range []string{sessionPreferenceKey(value), strings.TrimSpace(value.ID)} {
			if key == "" {
				continue
			}
			if err := s.store.DeleteSessionPreference(r.Context(), principal.UserID, key); err != nil {
				writeError(w, err)
				return
			}
		}
		value.Starred = false
	} else {
		if err := s.saveSessionPreference(r.Context(), principal.UserID, value, preference); err != nil {
			writeError(w, err)
			return
		}
		value.Starred = preference.Starred
		if preference.DisplayName != "" {
			value.DisplayName = preference.DisplayName
			value.DisplayNameSource = session.DisplayNameSourceCustom
		}
	}
	writeJSON(w, http.StatusOK, value)
}

// deleteSession permanently removes a stopped session. The provider transcript
// lives on the owning workstation, so a remote session is deleted through its
// Daemon; a local session is deleted by the in-process manager. A running
// session is refused because its files are still open.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, location, err := s.resolveSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if location == sessionLocationDaemonHistory || s.manager == nil || !s.manager.CanManageSessions() {
		// A canonical id that is not in the Server store resolves to a
		// synthetic stopped row, so consult the owning Daemon's live summary
		// before trusting it.
		value = s.daemons.effectiveSession(value)
	}
	if isLiveSession(value) {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session is running; stop it before deleting"))
		return
	}
	if location == sessionLocationDaemonHistory || s.manager == nil || !s.manager.CanManageSessions() {
		if !s.daemons.hasRoute(value.ID) {
			writeErrorStatus(w, http.StatusBadGateway, fmt.Errorf("session owner is offline; cannot delete its files"))
			return
		}
		if err := s.daemons.deleteSession(r.Context(), value); err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
	} else if err := s.manager.DeleteSession(r.Context(), value); err != nil {
		writeErrorStatus(w, http.StatusConflict, err)
		return
	}
	// Remove what the Server still holds for this session: the persisted row,
	// the user's preference and the hub's cached route/history entry.
	if err := s.store.DeleteSession(r.Context(), value.ID); err != nil {
		writeError(w, err)
		return
	}
	s.daemons.forgetSession(value.ID)
	// The session is gone for everyone, so drop every user's alias/star for it
	// instead of leaving orphan rows. Both key shapes are cleared because the
	// preference may have been saved before the native URI was resolved.
	for _, key := range []string{sessionPreferenceKey(value), strings.TrimSpace(value.ID)} {
		_ = s.store.DeleteSessionPreferenceByKey(r.Context(), key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	value, location, err := s.resolveSession(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	// Remote daemon sessions may have been persisted before the daemon sent
	// their history path/workspace metadata. Do not turn that temporary
	// incompleteness into a permanently empty transcript: the daemon can still
	// resolve the canonical session from its route/history catalog.
	remoteDaemonSession := s.manager == nil && value.NativeSessionURI() != ""
	if !value.Capabilities.CanReadHistory && !remoteDaemonSession {
		writeJSON(w, http.StatusOK, []event.Event{})
		return
	}
	limit := parseHistoryLimit(r.URL.Query().Get("limit"))
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	var values []event.Event
	if location == sessionLocationLocalHistory {
		values, err = s.manager.HistoryForSession(r.Context(), value, 0)
	} else if location == sessionLocationDaemonHistory {
		values, err = s.requestDaemonHistory(r.Context(), sessionID, historyRequestLimit(limit), before)
	} else if s.manager != nil && s.manager.CanManageSessions() {
		values, err = s.manager.History(r.Context(), sessionID, 0)
	} else {
		values, err = s.requestDaemonHistory(r.Context(), sessionID, historyRequestLimit(limit), before)
	}
	if err != nil {
		if location == sessionLocationDaemonHistory || s.manager == nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
		} else {
			writeError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, pageHistory(values, limit, before))
}

func parseHistoryLimit(value string) int {
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > 5000 {
		return 0
	}
	return limit
}

func historyRequestLimit(limit int) int {
	if limit > 0 {
		return limit
	}
	return 1000
}

func pageHistory(values []event.Event, limit int, before string) []event.Event {
	if before != "" {
		for index := range values {
			if values[index].ID == before {
				values = values[:index]
				break
			}
		}
	}
	if limit > 0 && len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func (s *Server) requestDaemonHistory(ctx context.Context, sessionID string, limit int, before string) ([]event.Event, error) {
	frame, err := s.daemons.request(ctx, sessionID, protocol.SessionHistoryRequest, protocol.HistoryRequestPayload{SessionID: sessionID, Limit: limit, Before: before}, protocol.SessionHistoryResponse)
	if err != nil {
		return nil, err
	}
	var response protocol.HistoryResponsePayload
	if err := protocol.DecodePayload(frame, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if len(response.Events) == 0 {
		return []event.Event{}, nil
	}
	var values []event.Event
	if err := json.Unmarshal(response.Events, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	value, _, err := s.resolveSession(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !value.Capabilities.CanStream {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session does not provide a live event stream"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorStatus(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	var ch <-chan event.Event
	var unsubscribe func()
	if s.manager != nil && s.manager.CanManageSessions() {
		ch, unsubscribe = s.manager.Subscribe(value.CoordinationID)
	} else {
		ch, unsubscribe = s.daemons.Subscribe(sessionID)
	}
	defer unsubscribe()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(15 * time.Second):
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		case item, ok := <-ch:
			if !ok {
				return
			}
			if item.SessionID != sessionID {
				continue
			}
			payload, _ := json.Marshal(item)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

func (s *Server) attachAddr(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, _, err := s.resolveSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !value.Capabilities.CanAttach {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session does not provide a terminal attachment"))
		return
	}
	if s.manager != nil && s.manager.CanManageSessions() {
		addr, err := s.manager.AttachAddr(id)
		if err != nil {
			writeErrorStatus(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"socket": addr})
		return
	}
	frame, err := s.daemons.request(r.Context(), id, protocol.AttachRequest, protocol.AttachPayload{SessionID: id}, protocol.AttachResponse)
	if err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	var response protocol.AttachPayload
	if err := protocol.DecodePayload(frame, &response); err != nil {
		writeErrorStatus(w, http.StatusBadGateway, err)
		return
	}
	if response.Error != "" {
		writeErrorStatus(w, http.StatusBadGateway, errors.New(response.Error))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"socket": response.Socket})
}

func (s *Server) ptySnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, _, err := s.resolveSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !value.Capabilities.CanReadTerminal {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session does not provide terminal snapshots"))
		return
	}
	if s.manager == nil || !s.manager.CanManageSessions() {
		frame, err := s.daemons.request(r.Context(), id, protocol.SnapshotRequest, protocol.SnapshotPayload{SessionID: id}, protocol.SnapshotResponse)
		if err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		var response protocol.SnapshotPayload
		if err := protocol.DecodePayload(frame, &response); err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		if response.Error != "" {
			writeErrorStatus(w, http.StatusBadGateway, errors.New(response.Error))
			return
		}
		writeRawJSON(w, http.StatusOK, response.Snapshot)
		return
	}
	snapshot, err := s.manager.Snapshot(id)
	if err != nil {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
func (s *Server) createMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	value, _, err := s.resolveSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	principal, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusUnauthorized, err)
		return
	}
	if !value.Capabilities.CanSendInput {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session is not running; resume it before sending input"))
		return
	}
	var input struct {
		Content string `json:"content"`
		ReplyTo string `json:"reply_to"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	input.Content = strings.TrimSpace(input.Content)
	if input.Content == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("content is required"))
		return
	}
	msg := message.Message{ID: newID("msg"), CoordinationID: value.CoordinationID, Sender: message.Endpoint{Type: "human", ID: principal.UserID}, Recipient: message.Endpoint{Type: "session", ID: id}, Content: input.Content, ReplyTo: input.ReplyTo, Status: message.StatusPending, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateMessage(r.Context(), msg); err != nil {
		writeError(w, err)
		return
	}
	var sendErr error
	if s.manager != nil && s.manager.CanManageSessions() {
		sendErr = s.manager.Send(r.Context(), value, msg)
	} else {
		sendErr = s.daemons.sessionInput(r.Context(), value, input.Content)
	}
	if sendErr != nil {
		_ = s.store.UpdateMessage(r.Context(), msg.ID, message.StatusFailed, sendErr.Error())
		writeErrorStatus(w, http.StatusConflict, sendErr)
		return
	}
	msg.Status = message.StatusSent
	writeJSON(w, http.StatusAccepted, msg)
}

func decodeJSON(r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request must contain one JSON object")
	}
	return nil
}
func writeRawJSON(w http.ResponseWriter, status int, value json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errForbidden) {
		writeErrorStatus(w, http.StatusForbidden, err)
		return
	}
	writeErrorStatus(w, http.StatusInternalServerError, err)
}
func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func newID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }
func newUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return newID("external")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
