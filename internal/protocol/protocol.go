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
	SessionRebind          = "session.rebind"
	EventBatch             = "event.batch"
	SessionHistoryRequest  = "session.history.request"
	SessionHistoryResponse = "session.history.response"
	SnapshotRequest        = "snapshot.request"
	SnapshotResponse       = "snapshot.response"
	AttachRequest          = "attach.request"
	AttachResponse         = "attach.response"
	SessionInput           = "session.input"
	SessionInputResult     = "session.input_result"
	SessionStop            = "session.stop"
	SessionStopResult      = "session.stop_result"
	SessionExit            = "session.exit"
	// SessionReport describes a message the injected Agent extension sends over
	// the local Daemon socket. It is provider-native evidence: the Agent itself
	// reports which session it is using, so no keystroke inference is needed.
	SessionReport = "session.report"
	Ack           = "ack"
	Error         = "error"
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
		SessionUpdate, SessionRebind, EventBatch, SessionHistoryRequest, SessionHistoryResponse,
		SnapshotRequest, SnapshotResponse, AttachRequest, AttachResponse, SessionInput, SessionInputResult,
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

// SessionList asks the local Daemon which sessions it owns or has discovered,
// so a terminal can pick one without knowing a canonical session id.
const SessionList = "session.list"

type SessionListRequest struct {
	Type string `json:"type"`
	// Workspace limits the answer to one directory. Empty lists every workspace
	// this Daemon knows about.
	Workspace string `json:"workspace,omitempty"`
	// Agent names a provider no Server accepts, and it is here on purpose: a
	// Daemon that predates session listing decodes this request as a wrapper
	// request and forwards it to the Server, where the unknown provider makes it
	// fail instead of quietly creating a session. Listing must never have side
	// effects, and a Daemon can only be upgraded while it is running.
	Agent string `json:"agent,omitempty"`
}

type SessionListEntry struct {
	SessionID string `json:"session_id"`
	// AgentSessionID is the provider's own identifier (pi://..., claude://...).
	// A terminal can pass either this or the canonical id back to the Daemon.
	AgentSessionID string `json:"agent_session_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	State          string `json:"state"`
	// Attachable reports whether the session is running with a PTY this Daemon
	// can hand to a terminal. A history session can only be picked from inside
	// the Agent (Pi and Claude both offer /resume).
	Attachable bool      `json:"attachable"`
	Source     string    `json:"source,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// SessionSnapshot asks the local Daemon for the screen a running session is
// showing. A terminal paints it before the live stream starts, so attaching does
// not begin with an empty screen.
const SessionSnapshot = "session.snapshot"

type SessionSnapshotRequest struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	// Agent carries the same guard as SessionListRequest: a Daemon that predates
	// this request must not mistake it for a wrapper request.
	Agent string `json:"agent,omitempty"`
}

type SessionSnapshotResponse struct {
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type SessionListResponse struct {
	Sessions []SessionListEntry `json:"sessions"`
	Error    string             `json:"error,omitempty"`
}

type SessionSummary struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	HistoryPath    string `json:"history_path,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID   string    `json:"claude_session_id,omitempty"`
	Workspace         string    `json:"workspace,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	DisplayNameSource string    `json:"display_name_source,omitempty"`
	Role              string    `json:"role,omitempty"`
	State             string    `json:"state"`
	Connection        string    `json:"connection,omitempty"`
	PID               int       `json:"pid,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

type HistorySessionSummary struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id"`
	HistoryPath    string `json:"history_path,omitempty"`
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
	SessionID      string   `json:"session_id,omitempty"`
	CoordinationID string   `json:"coordination_id,omitempty"`
	Workspace      string   `json:"workspace"`
	AgentArgs      []string `json:"agent_args,omitempty"`
	DisplayName    string   `json:"display_name,omitempty"`
	Role           string   `json:"role,omitempty"`
	Agent          string   `json:"agent,omitempty"`
	ResumeID       string   `json:"resume_id,omitempty"`
	HistoryPath    string   `json:"history_path,omitempty"`
	// DaemonID, when set, tells the receiving daemon which device the session
	// is expected to run on. The server only sends the frame to that daemon's
	// connection; the field lets the daemon verify the target defensively.
	DaemonID string `json:"daemon_id,omitempty"`
	// Terminal is the requesting terminal's identity, forwarded from the
	// wrapper; it becomes the Agent's TERM/COLORTERM environment.
	Terminal map[string]string `json:"terminal,omitempty"`
}

type SessionCreatedPayload struct {
	SessionID      string `json:"session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID string          `json:"claude_session_id,omitempty"`
	Workspace       string          `json:"workspace,omitempty"`
	PID             int             `json:"pid,omitempty"`
	HistoryPath     string          `json:"history_path,omitempty"`
	Capabilities    map[string]bool `json:"capabilities,omitempty"`
	Error           string          `json:"error,omitempty"`
}

type SessionRebindPayload struct {
	OldSessionID   string `json:"old_session_id"`
	NewSessionID   string `json:"new_session_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	HistoryPath    string `json:"history_path,omitempty"`
}

type SessionUpdatePayload struct {
	SessionID         string `json:"session_id"`
	DaemonID          string `json:"daemon_id,omitempty"`
	Agent             string `json:"agent,omitempty"`
	AgentSessionID    string `json:"agent_session_id,omitempty"`
	HistoryPath       string `json:"history_path,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	DisplayNameSource string `json:"display_name_source,omitempty"`
	// Deprecated compatibility field.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	State           string `json:"state"`
	Connection      string `json:"connection,omitempty"`
	PID             int    `json:"pid,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Attention       string `json:"attention,omitempty"`
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
	Before    string         `json:"before,omitempty"`
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

type AttachPayload struct {
	SessionID string `json:"session_id"`
	Socket    string `json:"socket,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SessionReportPayload is what the injected Agent extension reports to the
// local Daemon. Reasons follow the provider's own session lifecycle: "resume"
// and "new" before a switch, and "startup", "new", "resume", "fork" once the
// switch completed.
type SessionReportPayload struct {
	// HostID identifies the Session Host that owns the Agent. It is stable
	// across rebinds, unlike the canonical Session ID.
	HostID              string `json:"host_id"`
	Reason              string `json:"reason,omitempty"`
	SessionFile         string `json:"session_file,omitempty"`
	TargetSessionFile   string `json:"target_session_file,omitempty"`
	PreviousSessionFile string `json:"previous_session_file,omitempty"`
	SessionID           string `json:"session_id,omitempty"`
	SessionName         string `json:"session_name,omitempty"`
}

// SessionReportResponse acknowledges a report. Reporting is best effort, so the
// Agent extension never depends on the result.
type SessionReportResponse struct {
	Applied   bool   `json:"applied,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// WrapperRequest and WrapperResponse are the local Unix-socket protocol used
// by the installed `pi`/`claude` command wrappers. The wrapper never carries a
// Web-user token; the Daemon authenticates to the Server with its device
// credential and returns an attach address on the same workstation.
type WrapperRequest struct {
	SessionID   string   `json:"session_id,omitempty"`
	Workspace   string   `json:"workspace,omitempty"`
	AgentArgs   []string `json:"agent_args,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Role        string   `json:"role,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	Prompts     []string `json:"prompts,omitempty"`
	// Terminal carries the requesting terminal's identity (TERM, COLORTERM,
	// TERM_PROGRAM, TERM_PROGRAM_VERSION). The Daemon has no terminal of its
	// own, so without it a managed Agent cannot tell truecolor from 256 colors
	// or which emulator it is running under.
	Terminal map[string]string `json:"terminal,omitempty"`
}

type WrapperResponse struct {
	SessionID string `json:"session_id,omitempty"`
	Socket    string `json:"socket,omitempty"`
	Error     string `json:"error,omitempty"`
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
	Intentional   bool   `json:"intentional,omitempty"`
}

type AckPayload struct {
	AckMessageID string `json:"ack_message_id"`
}
type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}
