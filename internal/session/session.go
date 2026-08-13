package session

import "time"

const (
	StateCreated  = "created"
	StateStarting = "starting"
	StateRunning  = "running"
	StateWaiting  = "waiting"
	StateFailed   = "failed"
	StateStopped  = "stopped"
	StateStale    = "stale"
)

const (
	SourceManaged  = "managed"
	SourceExternal = "external"
	SourceProxy    = "proxy"
)

const (
	ConnectionObserved    = "observed"
	ConnectionStale       = "stale"
	ConnectionUnavailable = "unavailable"
)

// Session identifies an external Agent conversation managed or observed by Agora.
type Session struct {
	ID               string       `json:"id"`
	CoordinationID   string       `json:"coordination_id"`
	Agent            string       `json:"agent"`
	ExternalID       string       `json:"external_id"`
	ClaudeSessionID  string       `json:"claude_session_id,omitempty"`
	Workspace        string       `json:"workspace"`
	DisplayName      string       `json:"display_name"`
	Role             string       `json:"role"`
	State            string       `json:"state"`
	Source           string       `json:"source"`
	Connection       string       `json:"connection,omitempty"`
	ProcessID        int          `json:"process_id,omitempty"`
	SessionMetaPath  string       `json:"session_meta_path,omitempty"`
	HistoryPath      string       `json:"history_path,omitempty"`
	LastDiscoveredAt *time.Time   `json:"last_discovered_at,omitempty"`
	LastObservedAt   *time.Time   `json:"last_observed_at,omitempty"`
	LastError        string       `json:"last_error,omitempty"`
	Capabilities     Capabilities `json:"capabilities"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}
