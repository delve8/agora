package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const MaxFrameSize = 8 << 20

const (
	DaemonRegister         = "daemon.register"
	DaemonRegistered       = "daemon.registered"
	DaemonHeartbeat        = "daemon.heartbeat"
	DaemonHeartbeatAck     = "daemon.heartbeat_ack"
	DaemonResync           = "daemon.resync"
	ServerResyncRequest    = "server.resync_request"
	SessionCreate          = "session.create"
	SessionCreated         = "session.created"
	SessionUpdate          = "session.update"
	EventBatch             = "event.batch"
	SessionHistoryRequest  = "session.history.request"
	SessionHistoryResponse = "session.history.response"
	SnapshotRequest        = "snapshot.request"
	SnapshotResponse       = "snapshot.response"
	SessionInput           = "session.input"
	SessionInputResult     = "session.input_result"
	SessionStop            = "session.stop"
	SessionStopResult      = "session.stop_result"
	SessionExit            = "session.exit"
	Ack                    = "ack"
	Error                  = "error"
)

type Envelope struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	MessageID string          `json:"message_id,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(typ string, payload any) (Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: typ, MessageID: NewID("msg"), CreatedAt: time.Now().UTC(), Payload: body}, nil
}

func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func DecodePayload(e Envelope, target any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, target)
}

func ValidateType(typ string) error {
	switch typ {
	case DaemonRegister, DaemonRegistered, DaemonHeartbeat, DaemonHeartbeatAck,
		DaemonResync, ServerResyncRequest, SessionCreate, SessionCreated,
		SessionUpdate, EventBatch, SessionHistoryRequest, SessionHistoryResponse,
		SnapshotRequest, SnapshotResponse, SessionInput, SessionInputResult,
		SessionStop, SessionStopResult, SessionExit, Ack, Error:
		return nil
	default:
		return fmt.Errorf("unknown protocol message type %q", typ)
	}
}

type DaemonRegisterPayload struct {
	DaemonID     string          `json:"daemon_id"`
	Version      string          `json:"version"`
	Hostname     string          `json:"hostname"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

type HeartbeatPayload struct {
	DaemonID string    `json:"daemon_id"`
	At       time.Time `json:"at"`
}

type SessionSummary struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	State           string `json:"state"`
	Connection      string `json:"connection,omitempty"`
	PID             int    `json:"pid,omitempty"`
}

type HistorySessionSummary struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id"`
	// Deprecated compatibility field.
	ClaudeSessionID   string    `json:"claude_session_id,omitempty"`
	Workspace         string    `json:"workspace"`
	DisplayName       string    `json:"display_name"`
	DisplayNameSource string    `json:"display_name_source,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type ResyncPayload struct {
	DaemonID  string                  `json:"daemon_id"`
	Part      int                     `json:"part,omitempty"`
	Chunked   bool                    `json:"chunked,omitempty"`
	Final     bool                    `json:"final,omitempty"`
	Sessions  []SessionSummary        `json:"sessions,omitempty"`
	History   []HistorySessionSummary `json:"history,omitempty"`
	OutboxMin string                  `json:"outbox_min,omitempty"`
	OutboxMax string                  `json:"outbox_max,omitempty"`
	Gap       bool                    `json:"gap,omitempty"`
}

type SessionCreatePayload struct {
	SessionID      string `json:"session_id,omitempty"`
	CoordinationID string `json:"coordination_id,omitempty"`
	Workspace      string `json:"workspace"`
	DisplayName    string `json:"display_name,omitempty"`
	Role           string `json:"role,omitempty"`
	Agent          string `json:"agent,omitempty"`
	ResumeID       string `json:"resume_id,omitempty"`
}

type SessionCreatedPayload struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID string          `json:"claude_session_id,omitempty"`
	PID             int             `json:"pid,omitempty"`
	HistoryPath     string          `json:"history_path,omitempty"`
	Capabilities    map[string]bool `json:"capabilities,omitempty"`
	Error           string          `json:"error,omitempty"`
}

type SessionUpdatePayload struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	State           string `json:"state"`
	Connection      string `json:"connection,omitempty"`
	PID             int    `json:"pid,omitempty"`
	LastError       string `json:"last_error,omitempty"`
}

type EventBatchPayload struct {
	SessionID string          `json:"session_id"`
	Cursor    map[string]any  `json:"cursor,omitempty"`
	Events    json.RawMessage `json:"events,omitempty"`
	Gap       bool            `json:"gap,omitempty"`
}

type HistoryRequestPayload struct {
	SessionID string         `json:"session_id"`
	Cursor    map[string]any `json:"cursor,omitempty"`
	Since     string         `json:"since,omitempty"`
	Limit     int            `json:"limit"`
}

type HistoryResponsePayload struct {
	SessionID string          `json:"session_id"`
	Events    json.RawMessage `json:"events,omitempty"`
	Cursor    map[string]any  `json:"cursor,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type SnapshotPayload struct {
	SessionID string          `json:"session_id"`
	Snapshot  json.RawMessage `json:"snapshot,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type InputPayload struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

type InputResultPayload struct {
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

type StopPayload struct {
	SessionID string `json:"session_id"`
}

type StopResultPayload struct {
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

type ExitPayload struct {
	SessionID     string `json:"session_id"`
	State         string `json:"state"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Signal        string `json:"signal,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	DroppedEvents int    `json:"dropped_events,omitempty"`
}

type AckPayload struct {
	AckMessageID string `json:"ack_message_id"`
}
type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}
