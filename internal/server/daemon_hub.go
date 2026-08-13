package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/session"
)

const (
	defaultDaemonHeartbeatTimeout = 45 * time.Second
	maxDaemonFrameSize            = 1 << 20
)

type daemonHub struct {
	mu       sync.RWMutex
	devices  map[string]*daemonConnection
	routes   map[string]string
	seen     map[string]time.Time
	subs     map[string]map[chan event.Event]struct{}
	pending  map[string]chan protocol.Envelope
	timeout  time.Duration
	token    string
	upgrader websocket.Upgrader
}

type daemonConnection struct {
	id       string
	conn     *websocket.Conn
	send     chan protocol.Envelope
	lastSeen time.Time
	mu       sync.Mutex
	closed   bool
}

func newDaemonHub() *daemonHub {
	h := &daemonHub{
		devices: make(map[string]*daemonConnection),
		routes:  make(map[string]string),
		seen:    make(map[string]time.Time),
		subs:    make(map[string]map[chan event.Event]struct{}),
		pending: make(map[string]chan protocol.Envelope),
		timeout: defaultDaemonHeartbeatTimeout,
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
	if h.token != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
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
			registered = &daemonConnection{id: payload.DaemonID, conn: conn, send: make(chan protocol.Envelope, 128), lastSeen: time.Now().UTC()}
			h.register(registered)
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
	h.mu.Lock()
	c.lastSeen = time.Now().UTC()
	h.mu.Unlock()
	switch frame.Type {
	case protocol.DaemonHeartbeat:
		response, _ := protocol.NewEnvelope(protocol.DaemonHeartbeatAck, map[string]any{"at": time.Now().UTC()})
		response.RequestID = frame.RequestID
		return c.write(response)
	case protocol.DaemonResync:
		var payload protocol.ResyncPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		h.mu.Lock()
		for _, item := range payload.Sessions {
			h.routes[item.SessionID] = c.id
		}
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
			h.mu.Unlock()
		}
		return nil
	case protocol.EventBatch:
		return h.handleEventBatch(c, frame)
	case protocol.SessionInputResult, protocol.SessionHistoryResponse, protocol.SnapshotResponse:
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
			h.mu.Unlock()
		}
		return nil
	default:
		return fmt.Errorf("message type %q is not valid from daemon", frame.Type)
	}
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
	duplicate := frame.MessageID != "" && !h.seen[frame.MessageID].IsZero()
	if frame.MessageID != "" && !duplicate {
		h.seen[frame.MessageID] = time.Now().UTC()
	}
	subs := make([]chan event.Event, 0, len(h.subs[payload.SessionID]))
	for ch := range h.subs[payload.SessionID] {
		subs = append(subs, ch)
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
	for _, item := range payload.Events {
		item.SessionID = payload.SessionID
		for _, ch := range subs {
			select {
			case ch <- item:
			default:
			}
		}
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

func (h *daemonHub) sendToSession(sessionID string, frame protocol.Envelope) error {
	h.mu.RLock()
	daemonID := h.routes[sessionID]
	c := h.devices[daemonID]
	h.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("session %s is offline", sessionID)
	}
	return c.write(frame)
}

func (h *daemonHub) request(ctx context.Context, sessionID, typ string, payload any, responseType string) (protocol.Envelope, error) {
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
	if err := h.sendToSession(sessionID, frame); err != nil {
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
	default:
		return errors.New("daemon connection queue is full")
	}
	for {
		select {
		case next := <-c.send:
			if err := c.conn.WriteJSON(next); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (c *daemonConnection) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
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
