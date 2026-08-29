package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	// proxySessions holds proxy-registered sessions in memory so the session
	// list never has to read them back from the store.
	proxyMu       sync.Mutex
	proxySessions map[string]session.Session
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
	s := &Server{store: db, manager: manager, daemons: newDaemonHub(db, authConfig.Mode), auth: auth.NewAuthenticatorWithProvisioning(authConfig.Mode, validator, db, local, authConfig.Provisioning), proxySessions: make(map[string]session.Session)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/daemon/ws", s.daemons.serveHTTP)
	mux.HandleFunc("POST /api/daemon/pair", s.pairDaemon)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/devices/pair-codes", s.createPairCode)
	mux.HandleFunc("GET /api/devices", s.listDevices)
	mux.HandleFunc("POST /api/devices/{id}/revoke", s.revokeDevice)
	mux.HandleFunc("POST /api/devices/{id}/name", s.renameDevice)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/coordinations", s.createCoordination)
	mux.HandleFunc("GET /api/coordinations/{id}", s.getCoordination)
	mux.HandleFunc("POST /api/coordinations/{id}/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.resumeSession)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.stopSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.getEvents)
	mux.HandleFunc("GET /api/sessions/{id}/events/stream", s.streamEvents)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.createMessage)
	mux.HandleFunc("GET /api/sessions/{id}/attach", s.attachAddr)
	mux.HandleFunc("GET /api/sessions/{id}/pty/snapshot", s.ptySnapshot)
	mux.HandleFunc("POST /api/proxy/sessions", s.registerProxySession)
	mux.HandleFunc("POST /api/proxy/sessions/{id}/events", s.ingestProxyEvents)
	mux.HandleFunc("POST /api/proxy/sessions/{id}/exit", s.reportProxyExit)
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
	if value.Source == session.SourceProxy || value.Source == session.SourceExternal {
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

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/daemon/ws" || r.URL.Path == "/api/daemon/pair" || !strings.HasPrefix(r.URL.Path, "/api/") {
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
// daemons (daemon mode), the local manager (serve mode), and proxy sessions
// registered in memory. It never reads sessions back from the store, so stale
// persisted rows for daemons that are offline never surface.
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
	sessions = append(sessions, s.proxySessionList()...)
	return sessions, nil
}

// hideRevokedSessions is a defense-in-depth boundary for state responses. Hub
// cleanup normally removes revoked daemons before they can contribute rows, but
// filtering against the store also covers an in-flight refresh or a connection
// that was revoked concurrently with state construction.
func (s *Server) hideRevokedSessions(ctx context.Context, values []session.Session) ([]session.Session, error) {
	if s.store == nil || len(values) == 0 {
		return values, nil
	}
	principal, err := auth.RequirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := s.store.ListDevices(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	revoked := make(map[string]bool)
	for _, device := range devices {
		if device.RevokedAt != nil {
			revoked[device.ID] = true
		}
	}
	if len(revoked) == 0 {
		return values, nil
	}
	filtered := values[:0]
	for _, value := range values {
		daemonID := value.DaemonID
		if daemonID == "" {
			if identity, parseErr := session.ParseSessionID(value.ID); parseErr == nil {
				daemonID = identity.DaemonID
			}
		}
		if revoked[daemonID] {
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
	sessions, err = s.hideRevokedSessions(r.Context(), sessions)
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
		if key := sessionMergeKey(value); key != "" {
			sessionKeys[key] = index
		}
	}
	mergeDiscovered := func(value session.Session) {
		key := sessionMergeKey(value)
		if key != "" {
			if index, exists := sessionKeys[key]; exists {
				// Older Pi sessions may have used the bootstrap prompt `.` as
				// their display name. Replace that placeholder when the history
				// catalog now provides an explicit name or a workspace fallback.
				if value.Agent == "pi" && (strings.TrimSpace(sessions[index].DisplayName) == "" || strings.TrimSpace(sessions[index].DisplayName) == "." || session.IsGeneratedDisplayName(sessions[index].DisplayName) || isOpaqueSessionName(sessions[index].DisplayName, "pi")) && strings.TrimSpace(value.DisplayName) != "" {
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
		sessions = append(sessions, value)
		if key != "" {
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
	sessions, err = s.hideRevokedSessions(r.Context(), sessions)
	if err != nil {
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
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"coordination": coord, "sessions": sessions})
}

func sessionMergeKey(value session.Session) string {
	// The provider is already part of the canonical URI (claude://..., pi://...)
	// and this also treats the legacy "claude" and "claude-code" labels as the
	// same session.
	if uri := value.NativeSessionURI(); uri != "" {
		return uri
	}
	return value.ID
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
	sessions, err = s.hideRevokedSessions(r.Context(), sessions)
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
	remoteDaemonSession := s.manager == nil && value.Source != session.SourceProxy && value.NativeSessionURI() != ""
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
	} else if s.manager != nil && (s.manager.CanManageSessions() || value.Source == session.SourceProxy && value.HistoryPath != "") {
		values, err = s.manager.History(r.Context(), sessionID, 0)
	} else if value.Source != session.SourceProxy {
		values, err = s.requestDaemonHistory(r.Context(), sessionID, historyRequestLimit(limit), before)
	} else {
		values = []event.Event{}
	}
	if err != nil {
		if location == sessionLocationDaemonHistory || (s.manager == nil && value.Source != session.SourceProxy) {
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
	if s.manager != nil && s.manager.CanManageSessions() && value.Source != session.SourceProxy {
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
		var addr string
		var err error
		if value.Agent == "pi" {
			addr, err = s.manager.AttachPiAddr(id)
		} else {
			addr, err = s.manager.AttachAddr(id)
		}
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

func (s *Server) registerProxySession(w http.ResponseWriter, r *http.Request) {
	if !s.proxyRequestAllowed(r) {
		writeErrorStatus(w, http.StatusForbidden, fmt.Errorf("proxy endpoint requires a loopback request"))
		return
	}
	var input struct {
		Workspace   string `json:"workspace"`
		DisplayName string `json:"display_name"`
		WrapperPID  int    `json:"wrapper_pid"`
		RealPID     int    `json:"real_pid"`
		ResumeID    string `json:"resume_id"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	coordinations, err := s.store.ListCoordinations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	var coordinationID string
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
	workspace := strings.TrimSpace(input.Workspace)
	if workspace != "" {
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, err)
			return
		}
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{
		ID:                newID("sess"),
		CoordinationID:    coordinationID,
		Agent:             "claude-code",
		ExternalID:        strings.TrimSpace(input.ResumeID),
		ClaudeSessionID:   strings.TrimSpace(input.ResumeID),
		Workspace:         workspace,
		DisplayName:       displayName,
		DisplayNameSource: session.InitialDisplayNameSource(displayName),
		State:             session.StateRunning,
		Source:            session.SourceProxy,
		Connection:        session.ConnectionObserved,
		ProcessID:         input.RealPID,
		Capabilities: session.Capabilities{
			CanObserve: true,
			CanStream:  true,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if value.ProcessID == 0 {
		value.ProcessID = input.WrapperPID
	}
	if value.ClaudeSessionID != "" && s.manager != nil {
		value.HistoryPath = adapter.FindHistoryBySessionID("", value.ClaudeSessionID)
		value.Capabilities.CanReadHistory = value.HistoryPath != ""
	}
	if err := s.store.CreateSession(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	s.setProxySession(value)
	writeJSON(w, http.StatusCreated, value)
}

func (s *Server) ingestProxyEvents(w http.ResponseWriter, r *http.Request) {
	if !s.proxyRequestAllowed(r) {
		writeErrorStatus(w, http.StatusForbidden, fmt.Errorf("proxy endpoint requires a loopback request"))
		return
	}
	sessionID := r.PathValue("id")
	value, err := s.store.GetSession(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Source != session.SourceProxy {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session is not a proxy session"))
		return
	}
	var values []event.Event
	if err := decodeJSON(r, &values); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if len(values) > 256 {
		writeErrorStatus(w, http.StatusRequestEntityTooLarge, fmt.Errorf("event batch is too large"))
		return
	}
	for index := range values {
		values[index].SessionID = sessionID
		if values[index].Source == "" {
			values[index].Source = event.SourceStream
		}
		if values[index].ID == "" {
			identity := values[index].ExternalID
			if identity == "" {
				identity = values[index].RawJSON
			}
			if identity == "" {
				identity = values[index].Kind + "\x00" + values[index].Content
			}
			values[index].ID = stableProxyEventID(sessionID, identity)
		}
		if values[index].CreatedAt.IsZero() {
			values[index].CreatedAt = time.Now().UTC()
		}
	}
	inserted := s.daemons.Publish(sessionID, values)
	writeJSON(w, http.StatusOK, map[string]int{"ingested": inserted})
}

func (s *Server) reportProxyExit(w http.ResponseWriter, r *http.Request) {
	if !s.proxyRequestAllowed(r) {
		writeErrorStatus(w, http.StatusForbidden, fmt.Errorf("proxy endpoint requires a loopback request"))
		return
	}
	sessionID := r.PathValue("id")
	value, err := s.store.GetSession(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Source != session.SourceProxy {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session is not a proxy session"))
		return
	}
	var input struct {
		State         string `json:"state"`
		LastError     string `json:"last_error"`
		ExitCode      int    `json:"exit_code"`
		Signal        string `json:"signal"`
		DroppedEvents int    `json:"dropped_events"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if input.State != session.StateFailed {
		input.State = session.StateStopped
	}
	now := time.Now().UTC()
	value.State = input.State
	value.Connection = session.ConnectionStale
	value.LastObservedAt = &now
	value.LastError = input.LastError
	if err := s.store.UpdateSessionObservation(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	s.setProxySession(value)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "exit_code": input.ExitCode, "signal": input.Signal, "dropped_events": input.DroppedEvents})
}

func (s *Server) setProxySession(value session.Session) {
	s.proxyMu.Lock()
	s.proxySessions[value.ID] = value
	s.proxyMu.Unlock()
}

// proxySessionList returns the proxy sessions registered in memory, dropping
// entries whose process has exited.
func (s *Server) proxySessionList() []session.Session {
	s.proxyMu.Lock()
	values := make([]session.Session, 0, len(s.proxySessions))
	for id, value := range s.proxySessions {
		if value.ProcessID > 0 && !adapter.ProcessAlive(value.ProcessID) {
			value.State = session.StateStopped
			value.Connection = session.ConnectionUnavailable
			value.ProcessID = 0
			value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || value.ClaudeSessionID != "", CanResume: value.ClaudeSessionID != "" && value.Workspace != ""}
			s.proxySessions[id] = value
		}
		values = append(values, value)
	}
	s.proxyMu.Unlock()
	return values
}

func (s *Server) proxyRequestAllowed(r *http.Request) bool {
	if token := os.Getenv("AGORA_PROXY_TOKEN"); token != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
func stableProxyEventID(sessionID, identity string) string {
	var hash uint64 = 14695981039346656037
	for _, value := range []byte(sessionID + "\x00" + identity) {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return fmt.Sprintf("evt-%x", hash)
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
