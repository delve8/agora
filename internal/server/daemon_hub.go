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
	"path/filepath"
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
	mu                 sync.RWMutex
	store              *store.Store
	authMode           string
	devices            map[string]*daemonConnection
	routes             map[string]string
	sessions           map[string]map[string]protocol.SessionSummary
	history            map[string]map[string]protocol.HistorySessionSummary
	seen               map[string]time.Time
	seenOrder          []string
	subs               map[string]map[chan event.Event]struct{}
	resyncParts        map[string]resyncAccumulator
	pending            map[string]chan protocol.Envelope
	timeout            time.Duration
	token              string
	onEvents           func(string, []event.Event)
	onSessionAttention func(string, string)
	onSessionExit      func(string, int, string, bool)
	upgrader           websocket.Upgrader
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
			if h.store != nil {
				revoked, revokeErr := h.store.IsDeviceRevoked(r.Context(), payload.DaemonID)
				if revokeErr != nil {
					_ = writeEnvelope(conn, errorEnvelope("register_failed", "unable to validate daemon", frame.RequestID))
					return
				}
				if revoked {
					_ = writeEnvelope(conn, errorEnvelope("invalid_register", "daemon has been revoked", frame.RequestID))
					return
				}
			}
			registered = &daemonConnection{id: payload.DaemonID, userID: device.UserID, conn: conn, send: make(chan protocol.Envelope, 128), lastSeen: time.Now().UTC()}

			if h.authMode == "logto" && h.store != nil {
				_ = h.store.TouchDevice(r.Context(), payload.DaemonID, time.Now().UTC())
				// Backfill a friendly alias for devices paired without a name, so
				// the web device list and session badges show the hostname instead
				// of the raw device id. Subsequent reconnects skip this because
				// device.Name is then non-empty.
				if strings.TrimSpace(device.Name) == "" && strings.TrimSpace(payload.Hostname) != "" {
					_ = h.store.UpdateDeviceName(r.Context(), payload.DaemonID, sanitizeDeviceName(payload.Hostname))
				}
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

// disconnectDaemon closes the current connection for a daemon and runs the
// same in-memory cleanup as an ordinary websocket disconnect. It is safe to
// call when the daemon is already offline.
func (h *daemonHub) disconnectDaemon(daemonID string) {
	h.mu.RLock()
	connection := h.devices[daemonID]
	h.mu.RUnlock()
	if connection != nil {
		h.remove(connection)
	}
}

// isConnected reports whether a daemon with the given id is currently
// connected to the hub.
func (h *daemonHub) isConnected(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.devices[id] != nil
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
		// A provider history scan and managed-session rekey can cross in flight.
		// Reconcile the two views while installing a resync, so one native
		// session cannot be exposed as separate TUI and history rows even when
		// their temporary Agora IDs differ.
		for historyID, item := range history {
			for _, live := range sessions {
				if (item.AgentSessionID != "" && item.AgentSessionID == live.AgentSessionID) ||
					(item.Agent != "" && item.Agent == live.Agent && item.HistoryPath != "" && item.HistoryPath == live.HistoryPath) {
					delete(history, historyID)
					if h.routes[historyID] == c.id {
						delete(h.routes, historyID)
					}
					break
				}
			}
		}
		h.sessions[c.id] = sessions
		h.history[c.id] = history
		h.mu.Unlock()
		return nil
	case protocol.SessionRebind:
		var payload protocol.SessionRebindPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		if payload.OldSessionID == "" || payload.NewSessionID == "" || payload.OldSessionID == payload.NewSessionID {
			return errors.New("session rebind requires distinct old and new session ids")
		}
		identity, err := session.ParseSessionID(payload.NewSessionID)
		if err != nil || identity.DaemonID != c.id {
			if err == nil {
				err = fmt.Errorf("session %s is not owned by daemon %s", payload.NewSessionID, c.id)
			}
			return err
		}
		if payload.Agent != "" && payload.Agent != identity.Agent {
			return fmt.Errorf("session rebind agent %q does not match %q", payload.Agent, identity.Agent)
		}
		if payload.AgentSessionID != "" && payload.AgentSessionID != identity.AgentSessionID {
			return fmt.Errorf("session rebind native id %q does not match %q", payload.AgentSessionID, identity.AgentSessionID)
		}
		var stored session.Session
		if h.store != nil {
			value, getErr := h.store.GetSession(context.Background(), payload.OldSessionID)
			if getErr != nil {
				return getErr
			}
			value.ID = payload.NewSessionID
			value.DaemonID = c.id
			value.Agent = identity.Agent
			value.AgentSessionID = identity.AgentSessionID
			if payload.HistoryPath != "" {
				value.HistoryPath = payload.HistoryPath
			}
			if rekeyErr := h.store.RekeySession(context.Background(), payload.OldSessionID, value); rekeyErr != nil {
				return rekeyErr
			}
			stored = value
		}
		h.mu.Lock()
		old := h.sessions[c.id][payload.OldSessionID]
		if h.store != nil && old.SessionID == "" {
			old.SessionID = stored.ID
			old.DaemonID = stored.DaemonID
			old.Agent = stored.Agent
			old.AgentSessionID = stored.AgentSessionID
			old.HistoryPath = stored.HistoryPath
		}
		delete(h.sessions[c.id], payload.OldSessionID)
		delete(h.routes, payload.OldSessionID)
		old.SessionID = payload.NewSessionID
		old.Agent = identity.Agent
		old.AgentSessionID = identity.AgentSessionID
		if payload.HistoryPath != "" {
			old.HistoryPath = payload.HistoryPath
		}
		old.DaemonID = c.id
		if h.sessions[c.id] == nil {
			h.sessions[c.id] = make(map[string]protocol.SessionSummary)
		}
		h.sessions[c.id][payload.NewSessionID] = old
		h.routes[payload.NewSessionID] = c.id
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
			if payload.HistoryPath != "" {
				existing.HistoryPath = payload.HistoryPath
			}
			if payload.DisplayName != "" {
				existing.DisplayName = payload.DisplayName
				existing.DisplayNameSource = payload.DisplayNameSource
			}
			if payload.ClaudeSessionID != "" {
				existing.ClaudeSessionID = payload.ClaudeSessionID
			}
			if payload.State != "" {
				existing.State = payload.State
			}
			if payload.Connection != "" {
				existing.Connection = payload.Connection
			}
			// The update may arrive after session.created. Preserve the PID from
			// that response when an intermediate running update omits it; only a
			// terminal state is allowed to clear a PID explicitly.
			if payload.PID > 0 || payload.State == session.StateStopped || payload.State == session.StateFailed || payload.State == session.StateStale {
				existing.PID = payload.PID
			}
			h.sessions[c.id][payload.SessionID] = existing
			// A session can be advertised as history before its managed PTY has
			// completed native identity reconciliation. Once the live update
			// arrives, remove the matching history alias immediately instead of
			// waiting for the next full resync.
			h.removeHistoryAliasLocked(c.id, existing)
			h.mu.Unlock()
			if payload.Attention != "" && h.onSessionAttention != nil {
				h.onSessionAttention(payload.SessionID, payload.Attention)
			}
		}
		return nil
	case protocol.EventBatch:
		return h.handleEventBatch(c, frame)
	case protocol.SessionInputResult, protocol.SessionStopResult, protocol.SessionHistoryResponse, protocol.SnapshotResponse, protocol.AttachResponse, protocol.SessionCreated:
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
			if h.onSessionExit != nil && payload.ExitCode != 0 && !payload.Intentional {
				h.onSessionExit(payload.SessionID, payload.ExitCode, payload.LastError, payload.Intentional)
			}
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
	fresh := h.Publish(payload.SessionID, payload.Events)
	if fresh > 0 && h.onEvents != nil {
		h.onEvents(payload.SessionID, payload.Events)
	}
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
	if identity.Agent == "pi" && isOpaqueSessionName(value.DisplayName, identity.Agent) {
		value.DisplayName = friendlySessionName(identity.Agent, value.Workspace)
	}
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
	summary, live := h.sessions[identity.DaemonID][value.ID]
	if live && summary.ClaudeSessionID != "" {
		value.ClaudeSessionID = summary.ClaudeSessionID
	}
	if live && summary.AgentSessionID != "" {
		value.AgentSessionID = summary.AgentSessionID
	}
	if live && summary.HistoryPath != "" {
		value.HistoryPath = summary.HistoryPath
	}
	if live && strings.TrimSpace(summary.Workspace) != "" {
		value.Workspace = summary.Workspace
	}
	if live && strings.TrimSpace(summary.DisplayName) != "" && !isOpaqueSessionName(summary.DisplayName, identity.Agent) {
		value.DisplayName = summary.DisplayName
		value.DisplayNameSource = summary.DisplayNameSource
	}
	if !live {
		// The server store can contain a row created before the daemon learned
		// its Pi history path. Enrich that row from the daemon's history catalog;
		// otherwise getEvents would see CanReadHistory=false and return an empty
		// transcript even though the session is visible in /api/state.
		if history, exists := h.history[identity.DaemonID][value.ID]; exists {
			if history.AgentSessionID != "" {
				value.AgentSessionID = history.AgentSessionID
			}
			if history.HistoryPath != "" {
				value.HistoryPath = history.HistoryPath
			}
			if history.Workspace != "" {
				value.Workspace = history.Workspace
			}
			if history.DisplayName != "" && !isOpaqueSessionName(history.DisplayName, identity.Agent) {
				value.DisplayName = history.DisplayName
				value.DisplayNameSource = history.DisplayNameSource
			}
		}
	}
	if isOpaqueSessionName(value.DisplayName, identity.Agent) {
		value.DisplayName = friendlySessionName(identity.Agent, value.Workspace)
	}
	if live && (summary.State == session.StateRunning || summary.State == session.StateWaiting || summary.State == session.StateStarting) && summary.PID > 0 {
		value.State = summary.State
		value.Connection = summary.Connection
		value.ProcessID = summary.PID
		value.Capabilities = liveAgentCapabilities(identity.Agent)
		return value
	}
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.ProcessID = 0
	value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || resumable, CanResume: value.AgentSessionID != "" && value.Workspace != ""}
	return value
}

func (h *daemonHub) liveSessions(coordinationID string) []session.Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	values := make([]session.Session, 0)
	for daemonID, summaries := range h.sessions {
		if h.devices[daemonID] == nil {
			continue
		}
		for _, summary := range summaries {
			values = append(values, liveSummarySession(summary, coordinationID))
		}
	}
	return values
}

func liveSummarySession(summary protocol.SessionSummary, coordinationID string) session.Session {
	agent := summary.Agent
	if agent == "" {
		agent = "claude"
	}
	agentSessionID := summary.AgentSessionID
	if agentSessionID == "" && summary.ClaudeSessionID != "" {
		agentSessionID = agent + "://" + summary.ClaudeSessionID
	}
	daemonID := summary.DaemonID
	if daemonID == "" {
		if identity, err := session.ParseSessionID(summary.SessionID); err == nil {
			daemonID = identity.DaemonID
		}
	}
	value := session.Session{
		ID: summary.SessionID, CoordinationID: coordinationID, DaemonID: daemonID,
		Agent: agent, AgentSessionID: agentSessionID, ClaudeSessionID: summary.ClaudeSessionID,
		Workspace: summary.Workspace, DisplayName: summary.DisplayName, DisplayNameSource: summary.DisplayNameSource, HistoryPath: summary.HistoryPath,
		Role: summary.Role, State: summary.State, Source: session.SourceManaged,
		Connection: summary.Connection, ProcessID: summary.PID,
		CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt,
	}
	if isOpaqueSessionName(value.DisplayName, agent) {
		value.DisplayName = friendlySessionName(agent, value.Workspace)
	}
	switch value.State {
	case session.StateRunning, session.StateWaiting, session.StateStarting:
		if value.ProcessID > 0 {
			value.Capabilities = liveAgentCapabilities(agent)
		}
	default:
		resumable := value.Workspace != ""
		value.Capabilities = session.Capabilities{CanReadHistory: resumable, CanResume: resumable}
	}
	return value
}

// upsertLiveSession records a full session in the hub's live map so a session
// created or resumed through the server is listed immediately, without waiting
// for the daemon to resync. Later SessionUpdate frames only adjust the state
// fields and preserve the metadata stored here.
func liveAgentCapabilities(agent string) session.Capabilities {
	// Pi and Claude both run their native TUI under a daemon-owned PTY. The
	// daemon's VT emulator can therefore serve a read-only terminal snapshot to
	// the Web UI as well as the native attach client.
	canAttach := agent == "claude" || agent == "claude-code" || agent == "pi"
	canReadTerminal := canAttach
	return session.Capabilities{CanStart: true, CanAttach: canAttach, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: canReadTerminal}
}

func (h *daemonHub) upsertLiveSession(value session.Session) {
	identity, err := session.ParseSessionID(value.ID)
	if err != nil {
		return
	}
	if value.AgentSessionID == "" {
		value.AgentSessionID = identity.AgentSessionID
	}
	h.mu.Lock()
	if h.sessions[identity.DaemonID] == nil {
		h.sessions[identity.DaemonID] = make(map[string]protocol.SessionSummary)
	}
	summary := protocol.SessionSummary{
		SessionID: value.ID, DaemonID: identity.DaemonID, Agent: value.Agent,
		AgentSessionID: value.AgentSessionID, ClaudeSessionID: value.ClaudeSessionID,
		Workspace: value.Workspace, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource,
		Role: value.Role, HistoryPath: value.HistoryPath,
		State: value.State, Connection: value.Connection, PID: value.ProcessID,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	h.sessions[identity.DaemonID][value.ID] = summary
	h.removeHistoryAliasLocked(identity.DaemonID, summary)
	h.mu.Unlock()
}

// removeHistoryAliasLocked removes history rows that identify the same native
// provider session as a live row. It must be called with h.mu held.
func (h *daemonHub) removeHistoryAliasLocked(daemonID string, live protocol.SessionSummary) {
	for historyID, history := range h.history[daemonID] {
		if (live.AgentSessionID != "" && history.AgentSessionID == live.AgentSessionID) ||
			(live.Agent != "" && live.Agent == history.Agent && live.HistoryPath != "" && history.HistoryPath == live.HistoryPath) {
			delete(h.history[daemonID], historyID)
			if h.routes[historyID] == daemonID {
				delete(h.routes, historyID)
			}
		}
	}
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
	name := summary.DisplayName
	if isOpaqueSessionName(name, agent) {
		name = friendlySessionName(agent, summary.Workspace)
	}
	return session.Session{ID: summary.SessionID, CoordinationID: coordinationID, DaemonID: summary.DaemonID, Agent: agent, AgentSessionID: agentSessionID, ClaudeSessionID: summary.ClaudeSessionID, ExternalID: summary.ClaudeSessionID, Workspace: summary.Workspace, HistoryPath: summary.HistoryPath, DisplayName: name, DisplayNameSource: summary.DisplayNameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: agentSessionID != "" && summary.Workspace != ""}, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt}
}

func isOpaqueSessionName(name, agent string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	return strings.HasPrefix(name, agent+"://") || strings.HasPrefix(name, "claude://") || strings.HasPrefix(name, "pi://")
}

func friendlySessionName(agent, workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace != "" {
		base := filepath.Base(filepath.Clean(workspace))
		if base != "" && base != "." && base != string(filepath.Separator) {
			if agent == "pi" {
				return "Pi · " + base
			}
			return base
		}
	}
	if agent == "pi" {
		return "Pi session"
	}
	return "New session"
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
	return h.requestConnection(ctx, connection, typ, payload, responseType)
}

// requestToDaemon sends a request frame to a specific connected daemon,
// failing fast when that daemon is offline instead of silently choosing
// another one.
func (h *daemonHub) requestToDaemon(ctx context.Context, daemonID, typ string, payload any, responseType string) (protocol.Envelope, error) {
	h.mu.RLock()
	connection := h.devices[daemonID]
	h.mu.RUnlock()
	if connection == nil {
		return protocol.Envelope{}, fmt.Errorf("daemon %s is offline", daemonID)
	}
	return h.requestConnection(ctx, connection, typ, payload, responseType)
}

// requestConnection is the shared send/timeout/response core for daemon
// requests; requestAny, requestToDaemon and request all resolve a connection
// and delegate here.
func (h *daemonHub) requestConnection(ctx context.Context, connection *daemonConnection, typ string, payload any, responseType string) (protocol.Envelope, error) {
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

func (h *daemonHub) createSession(ctx context.Context, payload protocol.SessionCreatePayload, targetDaemonID string) (protocol.SessionCreatedPayload, error) {
	var frame protocol.Envelope
	var err error
	if strings.TrimSpace(targetDaemonID) != "" {
		frame, err = h.requestToDaemon(ctx, targetDaemonID, protocol.SessionCreate, payload, protocol.SessionCreated)
	} else {
		frame, err = h.requestAny(ctx, protocol.SessionCreate, payload, protocol.SessionCreated)
	}
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
	return h.requestConnection(ctx, connection, typ, payload, responseType)
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
		DisplayName: value.DisplayName, Role: value.Role, Agent: identity.Agent, ResumeID: identity.AgentSessionID, HistoryPath: value.HistoryPath,
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
	value.Capabilities = liveAgentCapabilities(identity.Agent)
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
