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
	"os"
	"path/filepath"
	"strings"
	"time"

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
}

func New(addr string, db *store.Store, manager *runtime.Manager) *Server {
	s := &Server{store: db, manager: manager, daemons: newDaemonHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/daemon/ws", s.daemons.serveHTTP)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/coordinations", s.createCoordination)
	mux.HandleFunc("GET /api/coordinations/{id}", s.getCoordination)
	mux.HandleFunc("POST /api/coordinations/{id}/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.getEvents)
	mux.HandleFunc("GET /api/sessions/{id}/events/stream", s.streamEvents)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.createMessage)
	mux.HandleFunc("GET /api/sessions/{id}/attach", s.attachAddr)
	mux.HandleFunc("GET /api/sessions/{id}/pty/snapshot", s.ptySnapshot)
	mux.HandleFunc("POST /api/proxy/sessions", s.registerProxySession)
	mux.HandleFunc("POST /api/proxy/sessions/{id}/events", s.ingestProxyEvents)
	mux.HandleFunc("POST /api/proxy/sessions/{id}/exit", s.reportProxyExit)
	s.HTTP = &http.Server{Addr: addr, Handler: mux}
	return s
}

func (s *Server) Addr() string                       { return s.HTTP.Addr }
func (s *Server) ListenAndServe() error              { return s.HTTP.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.HTTP.Shutdown(ctx) }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	sessions, err := s.store.ListSessions(r.Context(), coord.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coordination": coord, "sessions": sessions})
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
	if s.manager != nil {
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
	if s.manager != nil {
		value, err := s.manager.CreateManagedSessionWithID(r.Context(), newID("sess"), coordinationID, workspace, input.DisplayName, input.Role)
		if err != nil {
			writeErrorStatus(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, value)
		return
	}
	value := session.Session{ID: newID("sess"), CoordinationID: coordinationID, Agent: "claude-code", Workspace: workspace, DisplayName: input.DisplayName, Role: input.Role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: session.Capabilities{CanStart: true, CanSendInput: true, CanStream: true, CanResume: true, CanReadHistory: true, CanObserve: true}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.store.CreateSession(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
	frame, err := protocol.NewEnvelope(protocol.SessionCreate, protocol.SessionCreatePayload{SessionID: value.ID, CoordinationID: coordinationID, Workspace: workspace, DisplayName: input.DisplayName, Role: input.Role})
	if err != nil || s.daemons == nil {
		writeErrorStatus(w, http.StatusServiceUnavailable, fmt.Errorf("daemon service is unavailable"))
		return
	}
	if err := s.daemons.broadcast(frame); err != nil {
		writeErrorStatus(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusAccepted, value)
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
	sessions, err := s.store.ListSessions(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coordination": value, "sessions": sessions})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	value, err := s.store.GetSession(r.Context(), r.PathValue("id"))
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
	if s.manager == nil {
		frame, err := s.daemons.request(r.Context(), sessionID, protocol.SessionHistoryRequest, protocol.HistoryRequestPayload{SessionID: sessionID, Limit: 1000}, protocol.SessionHistoryResponse)
		if err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		var response protocol.HistoryResponsePayload
		if err := protocol.DecodePayload(frame, &response); err != nil {
			writeErrorStatus(w, http.StatusBadGateway, err)
			return
		}
		if response.Error != "" {
			writeErrorStatus(w, http.StatusBadGateway, errors.New(response.Error))
			return
		}
		if len(response.Events) == 0 {
			response.Events = json.RawMessage("[]")
		}
		writeRawJSON(w, http.StatusOK, response.Events)
		return
	}
	values, err := s.store.ListEvents(r.Context(), sessionID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, values)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
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
	if s.manager != nil {
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
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	addr, err := s.manager.AttachAddr(id)
	if err != nil {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"socket": addr})
}

func (s *Server) ptySnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrorStatus(w, http.StatusNotFound, err)
			return
		}
		writeError(w, err)
		return
	}
	if s.manager == nil {
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
	value, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Source == session.SourceExternal || !value.Capabilities.CanSendInput {
		writeErrorStatus(w, http.StatusConflict, fmt.Errorf("session is observed-only; send input directly in Claude Code"))
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
	msg := message.Message{ID: newID("msg"), CoordinationID: value.CoordinationID, Sender: message.Endpoint{Type: "human", ID: "local"}, Recipient: message.Endpoint{Type: "session", ID: id}, Content: input.Content, ReplyTo: input.ReplyTo, Status: message.StatusPending, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateMessage(r.Context(), msg); err != nil {
		writeError(w, err)
		return
	}
	var sendErr error
	if s.manager != nil {
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
		displayName = "Claude Code Proxy"
	}
	now := time.Now().UTC()
	value := session.Session{
		ID:             newID("sess"),
		CoordinationID: coordinationID,
		Agent:          "claude-code",
		ExternalID:     strings.TrimSpace(input.ResumeID),
		Workspace:      workspace,
		DisplayName:    displayName,
		State:          session.StateRunning,
		Source:         session.SourceProxy,
		Connection:     session.ConnectionObserved,
		ProcessID:      input.RealPID,
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
	if err := s.store.CreateSession(r.Context(), value); err != nil {
		writeError(w, err)
		return
	}
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
	inserted := 0
	for _, item := range values {
		item.SessionID = sessionID
		if item.Source == "" {
			item.Source = event.SourceStream
		}
		if item.ExternalID != "" {
			exists, err := s.store.HasEvent(r.Context(), sessionID, item.Source, item.ExternalID)
			if err != nil {
				writeError(w, err)
				return
			}
			if exists {
				continue
			}
		}
		if item.ID == "" {
			item.ID = newID("evt")
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = time.Now().UTC()
		}
		if err := s.store.AppendEvent(r.Context(), item); err != nil {
			writeError(w, err)
			return
		}
		if err := s.manager.PublishEvent(r.Context(), sessionID, item); err != nil {
			writeError(w, err)
			return
		}
		inserted++
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "exit_code": input.ExitCode, "signal": input.Signal, "dropped_events": input.DroppedEvents})
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
