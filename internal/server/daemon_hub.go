package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/delve8/agora/internal/auth"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

const (
	defaultDaemonHeartbeatTimeout = 45 * time.Second
	maxDaemonFrameSize            = protocol.MaxFrameSize
	maxSeenEvents                 = 4096
)

type daemonHub struct {
	mu          sync.RWMutex
	store       *store.Store
	authMode    string
	devices     map[string]*daemonConnection
	routes      map[string]string
	sessions    map[string]map[string]protocol.SessionSummary
	history     map[string]map[string]protocol.HistorySessionSummary
	seen        map[string]time.Time
	seenOrder   []string
	subs        map[string]map[chan event.Event]struct{}
	resyncParts map[string]resyncAccumulator
	pending     map[string]chan protocol.Envelope
	timeout     time.Duration
	token       string
	upgrader    websocket.Upgrader
}

type resyncAccumulator struct {
	part     int
	sessions []protocol.SessionSummary
	history  []protocol.HistorySessionSummary
	gap      bool
}
type daemonConnection struct {
	id       string
	userID   string
	conn     *websocket.Conn
	send     chan protocol.Envelope
	lastSeen time.Time
	mu       sync.Mutex
	closed   bool
}

func newDaemonHub(args ...any) *daemonHub {
	var db *store.Store
	authMode := "local"
	if len(args) > 0 {
		db, _ = args[0].(*store.Store)
	}
	if len(args) > 1 {
		authMode, _ = args[1].(string)
	}
	h := &daemonHub{
		store:       db,
		authMode:    authMode,
		devices:     make(map[string]*daemonConnection),
		routes:      make(map[string]string),
		sessions:    make(map[string]map[string]protocol.SessionSummary),
		history:     make(map[string]map[string]protocol.HistorySessionSummary),
		seen:        make(map[string]time.Time),
		subs:        make(map[string]map[chan event.Event]struct{}),
		resyncParts: make(map[string]resyncAccumulator),
		pending:     make(map[string]chan protocol.Envelope),
		timeout:     defaultDaemonHeartbeatTimeout,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == "" || sameHostOrigin(r, origin)
			},
		},
	}
	h.token = strings.TrimSpace(os.Getenv("AGORA_DAEMON_TOKEN"))
	return h
}

func (h *daemonHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var device store.Device
	if h.authMode == "logto" {
		provided := auth.ExtractBearer(r.Header.Get("Authorization"))
		if provided == "" || h.store == nil {
			http.Error(w, "daemon authentication required", http.StatusUnauthorized)
			return
		}
		var err error
		device, err = h.store.GetDeviceByCredentialHash(r.Context(), store.HashSecret(provided))
		if err != nil || device.RevokedAt != nil {
			http.Error(w, "daemon authentication required", http.StatusUnauthorized)
			return
		}
	} else if h.token != "" {
		provided := auth.ExtractBearer(r.Header.Get("Authorization"))
		if provided == "" || hashCredential(provided) != hashCredential(h.token) {
			http.Error(w, "daemon authentication required", http.StatusUnauthorized)
			return
		}
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxDaemonFrameSize)
	registered := (*daemonConnection)(nil)
	defer func() {
		if registered != nil {
			h.remove(registered)
		}
		_ = conn.Close()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("agora server: daemon websocket read failed: %v", err)
			return
		}
		var frame protocol.Envelope
		if err := json.Unmarshal(data, &frame); err != nil {
			_ = writeEnvelope(conn, errorEnvelope("invalid_json", err.Error(), ""))
			continue
		}
		if err := protocol.ValidateType(frame.Type); err != nil {
			_ = writeEnvelope(conn, errorEnvelope("unknown_type", err.Error(), frame.RequestID))
			continue
		}
		if registered == nil {
			if frame.Type != protocol.DaemonRegister {
				_ = writeEnvelope(conn, errorEnvelope("not_registered", "daemon must register first", frame.RequestID))
				continue
			}
			var payload protocol.DaemonRegisterPayload
			if err := protocol.DecodePayload(frame, &payload); err != nil || strings.TrimSpace(payload.DaemonID) == "" {
				_ = writeEnvelope(conn, errorEnvelope("invalid_register", "daemon_id is required", frame.RequestID))
				continue
			}
			if h.authMode == "logto" && payload.DaemonID != device.ID {
				_ = writeEnvelope(conn, errorEnvelope("invalid_register", "daemon_id does not match credential", frame.RequestID))
				continue
			}
			registered = &daemonConnection{id: payload.DaemonID, userID: device.UserID, conn: conn, send: make(chan protocol.Envelope, 128), lastSeen: time.Now().UTC()}
			if h.authMode == "logto" && h.store != nil {
				_ = h.store.TouchDevice(r.Context(), payload.DaemonID, time.Now().UTC())
			}
			h.register(registered)
			go registered.writeLoop()
			response, _ := protocol.NewEnvelope(protocol.DaemonRegistered, map[string]any{"protocol_version": "1", "resync_required": true})
			response.RequestID = frame.RequestID
			if err := registered.write(response); err != nil {
				return
			}
			continue
		}
		if err := h.handleFrame(registered, frame); err != nil {
			_ = registered.write(errorEnvelope("protocol_error", err.Error(), frame.RequestID))
		}
	}
}

func (h *daemonHub) register(c *daemonConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.devices[c.id]; old != nil {
		old.close()
	}
	h.devices[c.id] = c
}

func (h *daemonHub) remove(c *daemonConnection) {
	h.mu.Lock()
	if current := h.devices[c.id]; current == c {
		delete(h.devices, c.id)
		delete(h.sessions, c.id)
		delete(h.history, c.id)
		for sid, did := range h.routes {
			if did == c.id {
				delete(h.routes, sid)
			}
		}
	}
	h.mu.Unlock()
	c.close()
}

func (h *daemonHub) handleFrame(c *daemonConnection, frame protocol.Envelope) error {
	log.Printf("agora server: frame %s request %s from daemon %s", frame.Type, frame.RequestID, c.id)
	h.mu.Lock()
	c.lastSeen = time.Now().UTC()
	h.mu.Unlock()
	switch frame.Type {
	case protocol.DaemonHeartbeat:
		if h.authMode == "logto" && h.store != nil {
			_ = h.store.TouchDevice(context.Background(), c.id, time.Now().UTC())
		}
		response, _ := protocol.NewEnvelope(protocol.DaemonHeartbeatAck, map[string]any{"at": time.Now().UTC()})
		response.RequestID = frame.RequestID
		return c.write(response)
	case protocol.DaemonResync:
		var payload protocol.ResyncPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		if payload.DaemonID != "" && payload.DaemonID != c.id {
			return fmt.Errorf("resync daemon id %q does not match connection %q", payload.DaemonID, c.id)
		}
		h.mu.Lock()
		acc := h.resyncParts[c.id]
		if !payload.Chunked {
			acc = resyncAccumulator{part: payload.Part, gap: payload.Gap, sessions: append([]protocol.SessionSummary(nil), payload.Sessions...), history: append([]protocol.HistorySessionSummary(nil), payload.History...)}
			payload.Final = true
		} else {
			if payload.Part == 0 {
				acc = resyncAccumulator{part: 0, gap: payload.Gap}
			}
			if payload.Part != acc.part {
				h.mu.Unlock()
				return fmt.Errorf("out-of-order resync part %d, expected %d", payload.Part, acc.part)
			}
			acc.sessions = append(acc.sessions, payload.Sessions...)
			acc.history = append(acc.history, payload.History...)
			acc.part++
			if !payload.Final {
				h.resyncParts[c.id] = acc
				h.mu.Unlock()
				return nil
			}
		}
		delete(h.resyncParts, c.id)
		for sid, did := range h.routes {
			if did == c.id {
				delete(h.routes, sid)
			}
		}
		sessions := make(map[string]protocol.SessionSummary, len(acc.sessions))
		history := make(map[string]protocol.HistorySessionSummary, len(acc.history))
		for _, item := range acc.sessions {
			sessions[item.SessionID] = item
			h.routes[item.SessionID] = c.id
		}
		for _, item := range acc.history {
			if item.SessionID != "" {
				history[item.SessionID] = item
				h.routes[item.SessionID] = c.id
			}
		}
		h.sessions[c.id] = sessions
		h.history[c.id] = history
		h.mu.Unlock()
		return nil
	case protocol.SessionUpdate:
		var payload protocol.SessionUpdatePayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		if payload.SessionID != "" {
			h.mu.Lock()
			h.routes[payload.SessionID] = c.id
			if h.sessions[c.id] == nil {
				h.sessions[c.id] = make(map[string]protocol.SessionSummary)
			}
			existing := h.sessions[c.id][payload.SessionID]
			existing.SessionID = payload.SessionID
			if payload.DaemonID != "" {
				existing.DaemonID = payload.DaemonID
			}
			if payload.Agent != "" {
				existing.Agent = payload.Agent
			}
			if payload.AgentSessionID != "" {
				existing.AgentSessionID = payload.AgentSessionID
			}
			if payload.ClaudeSessionID != "" {
				existing.ClaudeSessionID = payload.ClaudeSessionID
			}
			existing.State = payload.State
			existing.Connection = payload.Connection
			existing.PID = payload.PID
			h.sessions[c.id][payload.SessionID] = existing
			h.mu.Unlock()
		}
		return nil
	case protocol.EventBatch:
		return h.handleEventBatch(c, frame)
	case protocol.SessionInputResult, protocol.SessionStopResult, protocol.SessionHistoryResponse, protocol.SnapshotResponse, protocol.SessionCreated:
		log.Printf("agora server: response %s request %s", frame.Type, frame.RequestID)
		if frame.RequestID == "" {
			return nil
		}
		h.mu.RLock()
		response := h.pending[frame.RequestID]
		h.mu.RUnlock()
		if response != nil {
			select {
			case response <- frame:
			default:
			}
		}
		return nil
	case protocol.Ack:
		return nil
	case protocol.SessionExit:
		var payload protocol.ExitPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		if payload.SessionID != "" {
			h.mu.Lock()
			h.routes[payload.SessionID] = c.id
			if h.sessions[c.id] == nil {
				h.sessions[c.id] = make(map[string]protocol.SessionSummary)
			}
			current := h.sessions[c.id][payload.SessionID]
			current.SessionID = payload.SessionID
			current.State = session.StateStopped
			current.Connection = session.ConnectionUnavailable
			current.PID = 0
			h.sessions[c.id][payload.SessionID] = current
			h.mu.Unlock()
		}
		return nil
	default:
		return fmt.Errorf("message type %q is not valid from daemon", frame.Type)
	}
}

func (h *daemonHub) rememberSeenLocked(key string) bool {
	if key == "" {
		return false
	}
	if !h.seen[key].IsZero() {
		return true
	}
	h.seen[key] = time.Now().UTC()
	h.seenOrder = append(h.seenOrder, key)
	if len(h.seenOrder) > maxSeenEvents {
		oldest := h.seenOrder[0]
		h.seenOrder = h.seenOrder[1:]
		delete(h.seen, oldest)
	}
	return false
}

func eventSeenKey(sessionID string, item event.Event) string {
	if item.ID != "" {
		return sessionID + "\x00id\x00" + item.ID
	}
	if item.ExternalID != "" {
		return sessionID + "\x00" + item.Source + "\x00" + item.ExternalID
	}
	return ""
}

func (h *daemonHub) Publish(sessionID string, values []event.Event) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	fresh := 0
	for _, item := range values {
		item.SessionID = sessionID
		if h.rememberSeenLocked(eventSeenKey(sessionID, item)) {
			continue
		}
		fresh++
		for ch := range h.subs[sessionID] {
			select {
			case ch <- item:
			default:
			}
		}
	}
	return fresh
}

func (h *daemonHub) handleEventBatch(c *daemonConnection, frame protocol.Envelope) error {
	var payload struct {
		SessionID string        `json:"session_id"`
		Events    []event.Event `json:"events"`
	}
	if err := protocol.DecodePayload(frame, &payload); err != nil {
		return err
	}
	if payload.SessionID == "" {
		return errors.New("session_id is required")
	}
	h.mu.Lock()
	h.routes[payload.SessionID] = c.id
	duplicate := false
	if frame.MessageID != "" {
		duplicate = h.rememberSeenLocked("message\x00" + frame.MessageID)
	}
	h.mu.Unlock()
	if frame.MessageID != "" {
		ack, _ := protocol.NewEnvelope(protocol.Ack, protocol.AckPayload{AckMessageID: frame.MessageID})
		if err := c.write(ack); err != nil {
			return err
		}
	}
	if duplicate {
		return nil
	}
	h.Publish(payload.SessionID, payload.Events)
	return nil
}

func (h *daemonHub) Subscribe(sessionID string) (<-chan event.Event, func()) {
	ch := make(chan event.Event, 64)
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[chan event.Event]struct{})
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if subscribers := h.subs[sessionID]; subscribers != nil {
			delete(subscribers, ch)
			if len(subscribers) == 0 {
				delete(h.subs, sessionID)
			}
		}
		h.mu.Unlock()
		close(ch)
	}
}

func (h *daemonHub) daemonForSession(sessionID string) (*daemonConnection, error) {
	identity, err := session.ParseSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	connection := h.devices[identity.DaemonID]
	if connection == nil {
		return nil, fmt.Errorf("daemon %s is offline", identity.DaemonID)
	}
	return connection, nil
}
func (h *daemonHub) broadcast(frame protocol.Envelope) error {
	h.mu.RLock()
	connections := make([]*daemonConnection, 0, len(h.devices))
	for _, c := range h.devices {
		connections = append(connections, c)
	}
	h.mu.RUnlock()
	if len(connections) == 0 {
		return errors.New("no daemon is connected")
	}
	for _, c := range connections {
		if err := c.write(frame); err != nil {
			return err
		}
	}
	return nil
}

func (h *daemonHub) effectiveSession(value session.Session) session.Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	identity, err := session.ParseSessionID(value.ID)
	if err != nil {
		value.State = session.StateStopped
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		value.Capabilities = session.Capabilities{}
		return value
	}
	value.DaemonID = identity.DaemonID
	value.Agent = identity.Agent
	value.AgentSessionID = identity.AgentSessionID
	if identity.Agent == "claude" {
		value.ClaudeSessionID = strings.TrimPrefix(identity.AgentSessionID, "claude://")
	}
	resumable := value.Workspace != ""
	if h.devices[identity.DaemonID] == nil {
		value.State = session.StateStopped
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		value.Capabilities = session.Capabilities{CanReadHistory: resumable, CanResume: resumable}
		return value
	}
	summary, ok := h.sessions[identity.DaemonID][value.ID]
	if ok && summary.ClaudeSessionID != "" {
		value.ClaudeSessionID = summary.ClaudeSessionID
	}
	if ok && (summary.State == session.StateRunning || summary.State == session.StateWaiting || summary.State == session.StateStarting) && summary.PID > 0 {
		value.State = summary.State
		value.Connection = summary.Connection
		value.ProcessID = summary.PID
		value.Capabilities = session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
		return value
	}
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.ProcessID = 0
	value.Capabilities = session.Capabilities{CanReadHistory: resumable, CanResume: resumable}
	return value
}

func (h *daemonHub) historySessions(coordinationID string) []session.Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	values := make([]session.Session, 0)
	for daemonID, summaries := range h.history {
		if h.devices[daemonID] == nil {
			continue
		}
		for _, summary := range summaries {
			values = append(values, historySummarySession(summary, coordinationID))
		}
	}
	return values
}

func (h *daemonHub) historySession(id, coordinationID string) (session.Session, bool) {
	identity, err := session.ParseSessionID(id)
	if err != nil {
		h.mu.RLock()
		defer h.mu.RUnlock()
		daemonID := h.routes[id]
		if h.devices[daemonID] == nil {
			return session.Session{}, false
		}
		summary, ok := h.history[daemonID][id]
		if !ok {
			return session.Session{}, false
		}
		return historySummarySession(summary, coordinationID), true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.devices[identity.DaemonID] == nil {
		return session.Session{}, false
	}
	summary, ok := h.history[identity.DaemonID][id]
	if !ok {
		return session.Session{}, false
	}
	return historySummarySession(summary, coordinationID), true
}

func historySummarySession(summary protocol.HistorySessionSummary, coordinationID string) session.Session {
	agent := summary.Agent
	if agent == "" {
		agent = "claude"
	}
	agentSessionID := summary.AgentSessionID
	if agentSessionID == "" && summary.ClaudeSessionID != "" {
		agentSessionID = agent + "://" + summary.ClaudeSessionID
	}
	return session.Session{ID: summary.SessionID, CoordinationID: coordinationID, DaemonID: summary.DaemonID, Agent: agent, AgentSessionID: agentSessionID, ClaudeSessionID: summary.ClaudeSessionID, ExternalID: summary.ClaudeSessionID, Workspace: summary.Workspace, DisplayName: summary.DisplayName, DisplayNameSource: summary.DisplayNameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: agentSessionID != "" && summary.Workspace != ""}, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt}
}

func (h *daemonHub) hasRoute(sessionID string) bool {
	identity, err := session.ParseSessionID(sessionID)
	if err != nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.devices[identity.DaemonID] != nil
}

func (h *daemonHub) sendToSession(sessionID string, frame protocol.Envelope) error {
	identity, err := session.ParseSessionID(sessionID)
	if err != nil {
		return fmt.Errorf("invalid session %s: %w", sessionID, err)
	}
	h.mu.RLock()
	c := h.devices[identity.DaemonID]
	h.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("session %s is offline", sessionID)
	}
	return c.write(frame)
}

func (h *daemonHub) requestAny(ctx context.Context, typ string, payload any, responseType string) (protocol.Envelope, error) {
	h.mu.RLock()
	var connection *daemonConnection
	for _, candidate := range h.devices {
		connection = candidate
		break
	}
	h.mu.RUnlock()
	if connection == nil {
		return protocol.Envelope{}, errors.New("no daemon is connected")
	}
	frame, err := protocol.NewEnvelope(typ, payload)
	if err != nil {
		return protocol.Envelope{}, err
	}
	frame.RequestID = protocol.NewID("req")
	response := make(chan protocol.Envelope, 1)
	h.mu.Lock()
	h.pending[frame.RequestID] = response
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, frame.RequestID)
		h.mu.Unlock()
	}()
	if err := connection.write(frame); err != nil {
		return protocol.Envelope{}, err
	}
	// The peer may respond immediately; pending is installed before enqueue.
	timer := time.NewTimer(h.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	case <-timer.C:
		return protocol.Envelope{}, fmt.Errorf("daemon request timed out")
	case result := <-response:
		if result.Type != responseType {
			return protocol.Envelope{}, fmt.Errorf("unexpected daemon response %q", result.Type)
		}
		return result, nil
	}
}

func (h *daemonHub) createSession(ctx context.Context, payload protocol.SessionCreatePayload) (protocol.SessionCreatedPayload, error) {
	frame, err := h.requestAny(ctx, protocol.SessionCreate, payload, protocol.SessionCreated)
	if err != nil {
		return protocol.SessionCreatedPayload{}, err
	}
	var result protocol.SessionCreatedPayload
	if err := protocol.DecodePayload(frame, &result); err != nil {
		return protocol.SessionCreatedPayload{}, err
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

func (h *daemonHub) request(ctx context.Context, sessionID, typ string, payload any, responseType string) (protocol.Envelope, error) {
	connection, err := h.daemonForSession(sessionID)
	if err != nil {
		return protocol.Envelope{}, err
	}
	frame, err := protocol.NewEnvelope(typ, payload)
	if err != nil {
		return protocol.Envelope{}, err
	}
	frame.RequestID = protocol.NewID("req")
	response := make(chan protocol.Envelope, 1)
	h.mu.Lock()
	h.pending[frame.RequestID] = response
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, frame.RequestID)
		h.mu.Unlock()
	}()
	if err := connection.write(frame); err != nil {
		return protocol.Envelope{}, err
	}
	timer := time.NewTimer(h.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	case <-timer.C:
		return protocol.Envelope{}, fmt.Errorf("daemon request timed out")
	case result := <-response:
		if result.Type != responseType {
			return protocol.Envelope{}, fmt.Errorf("unexpected daemon response %q", result.Type)
		}
		return result, nil
	}
}
func (h *daemonHub) resumeSession(ctx context.Context, value session.Session) (session.Session, error) {
	identity, err := session.ParseSessionID(value.ID)
	if err != nil {
		return session.Session{}, fmt.Errorf("session %s does not have a canonical session id", value.ID)
	}
	value.DaemonID = identity.DaemonID
	value.Agent = identity.Agent
	value.AgentSessionID = identity.AgentSessionID
	if identity.Agent == "claude" {
		value.ClaudeSessionID = strings.TrimPrefix(identity.AgentSessionID, "claude://")
	}
	frame, err := h.request(ctx, value.ID, protocol.SessionCreate, protocol.SessionCreatePayload{
		SessionID: value.ID, CoordinationID: value.CoordinationID, Workspace: value.Workspace,
		DisplayName: value.DisplayName, Role: value.Role, Agent: identity.Agent, ResumeID: identity.AgentSessionID,
	}, protocol.SessionCreated)
	if err != nil {
		return session.Session{}, err
	}
	var result protocol.SessionCreatedPayload
	if err := protocol.DecodePayload(frame, &result); err != nil {
		return session.Session{}, err
	}
	if result.Error != "" {
		return session.Session{}, errors.New(result.Error)
	}
	if result.Workspace != "" {
		value.Workspace = result.Workspace
	}
	value.Source = session.SourceManaged
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.ProcessID = result.PID
	value.Capabilities = session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
	return value, nil
}

func (h *daemonHub) stopSession(ctx context.Context, value session.Session) error {
	frame, err := h.request(ctx, value.ID, protocol.SessionStop, protocol.StopPayload{SessionID: value.ID}, protocol.SessionStopResult)
	if err != nil {
		return err
	}
	var result protocol.StopResultPayload
	if err := protocol.DecodePayload(frame, &result); err != nil {
		return err
	}
	if !result.Accepted {
		return errors.New(result.Error)
	}
	return nil
}

func (h *daemonHub) sessionInput(ctx context.Context, value session.Session, content string) error {
	frame, err := h.request(ctx, value.ID, protocol.SessionInput, protocol.InputPayload{SessionID: value.ID, Content: content}, protocol.SessionInputResult)
	if err != nil {
		return err
	}
	var result protocol.InputResultPayload
	if err := protocol.DecodePayload(frame, &result); err != nil {
		return err
	}
	if !result.Accepted {
		return errors.New(result.Error)
	}
	return nil
}

func (c *daemonConnection) write(frame protocol.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("daemon connection is closed")
	}
	select {
	case c.send <- frame:
		return nil
	default:
		return errors.New("daemon connection queue is full")
	}
}

func (c *daemonConnection) writeLoop() {
	for frame := range c.send {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		if err := c.conn.WriteJSON(frame); err != nil {
			log.Printf("agora server: daemon %s write failed: %v", c.id, err)
			c.close()
			return
		}
	}
}

func (c *daemonConnection) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
	c.mu.Unlock()
}

func writeEnvelope(conn *websocket.Conn, frame protocol.Envelope) error {
	return conn.WriteJSON(frame)
}

func errorEnvelope(code, message, requestID string) protocol.Envelope {
	frame, _ := protocol.NewEnvelope(protocol.Error, protocol.ErrorPayload{Code: code, Message: message, RequestID: requestID})
	frame.RequestID = requestID
	return frame
}

func sameHostOrigin(r *http.Request, origin string) bool {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	return origin == "http://"+r.Host || origin == "https://"+r.Host
}

func hashCredential(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
