package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/delve8/agora/internal/event"
)

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
	SourceHistory  = "history"
)

const (
	ConnectionObserved    = "observed"
	ConnectionStale       = "stale"
	ConnectionUnavailable = "unavailable"
)

const (
	DisplayNameSourceInitial   = "initial"
	DisplayNameSourceCustom    = "custom"
	DisplayNameSourceFirstUser = "first_user"
	DisplayNameSourceAITitle   = "ai_title"
)

const canonicalSessionPrefix = "daemon/"

// SessionIdentity is the self-describing identity embedded in an Agora session ID.
type SessionIdentity struct {
	DaemonID       string
	Agent          string
	AgentSessionID string
}

func NewSessionID(daemonID, agent, agentSessionID string) (string, error) {
	identity, err := ParseAgentSessionID(agentSessionID)
	if err != nil {
		return "", err
	}
	if identity.Agent != agent {
		return "", fmt.Errorf("agent %q does not match session URI %q", agent, agentSessionID)
	}
	if err := validateSegment(daemonID, "daemon id"); err != nil {
		return "", err
	}
	if err := validateSegment(agent, "agent"); err != nil {
		return "", err
	}
	return canonicalSessionPrefix + daemonID + "/" + agentSessionID, nil
}

func ParseSessionID(id string) (SessionIdentity, error) {
	if !strings.HasPrefix(id, canonicalSessionPrefix) {
		return SessionIdentity{}, fmt.Errorf("invalid session id %q", id)
	}
	parts := strings.SplitN(strings.TrimPrefix(id, canonicalSessionPrefix), "/", 2)
	if len(parts) != 2 {
		return SessionIdentity{}, fmt.Errorf("invalid session id %q", id)
	}
	uri, err := ParseAgentSessionID(parts[1])
	if err != nil {
		return SessionIdentity{}, err
	}
	if err := validateSegment(parts[0], "daemon id"); err != nil {
		return SessionIdentity{}, err
	}
	return SessionIdentity{DaemonID: parts[0], Agent: uri.Agent, AgentSessionID: parts[1]}, nil
}

func ParseAgentSessionID(value string) (SessionIdentity, error) {
	parts := strings.SplitN(strings.TrimSpace(value), "://", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return SessionIdentity{}, fmt.Errorf("invalid agent session URI %q", value)
	}
	if err := validateSegment(parts[0], "agent"); err != nil {
		return SessionIdentity{}, err
	}
	if strings.ContainsAny(parts[1], "/\\?#") {
		return SessionIdentity{}, fmt.Errorf("invalid agent session URI %q", value)
	}
	return SessionIdentity{Agent: parts[0], AgentSessionID: value}, nil
}

func validateSegment(value, label string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#:\t\r\n") {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	return nil
}

// Session identifies an external Agent conversation managed or observed by Agora.
type Session struct {
	ID             string `json:"id"`
	CoordinationID string `json:"coordination_id"`
	DaemonID       string `json:"daemon_id,omitempty"`
	Agent          string `json:"agent"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Deprecated compatibility fields. New code must use AgentSessionID.
	ExternalID        string       `json:"external_id,omitempty"`
	ClaudeSessionID   string       `json:"claude_session_id,omitempty"`
	Workspace         string       `json:"workspace"`
	DisplayName       string       `json:"display_name"`
	DisplayNameSource string       `json:"display_name_source,omitempty"`
	Role              string       `json:"role"`
	State             string       `json:"state"`
	Source            string       `json:"source"`
	Connection        string       `json:"connection,omitempty"`
	ProcessID         int          `json:"process_id,omitempty"`
	SessionMetaPath   string       `json:"session_meta_path,omitempty"`
	HistoryPath       string       `json:"history_path,omitempty"`
	LastDiscoveredAt  *time.Time   `json:"last_discovered_at,omitempty"`
	LastObservedAt    *time.Time   `json:"last_observed_at,omitempty"`
	LastError         string       `json:"last_error,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

func (s Session) NativeSessionURI() string {
	if s.AgentSessionID != "" {
		return s.AgentSessionID
	}
	if s.ClaudeSessionID != "" {
		return "claude://" + s.ClaudeSessionID
	}
	return s.ExternalID
}

func (s Session) Identity() (SessionIdentity, error) {
	if value, err := ParseSessionID(s.ID); err == nil {
		return value, nil
	}
	if s.DaemonID != "" && s.Agent != "" && s.NativeSessionURI() != "" {
		return SessionIdentity{DaemonID: s.DaemonID, Agent: s.Agent, AgentSessionID: s.NativeSessionURI()}, nil
	}
	return SessionIdentity{}, fmt.Errorf("session %q has no canonical identity", s.ID)
}

func IsGeneratedDisplayName(value string) bool {
	switch strings.TrimSpace(value) {
	case "", "New session", "Claude Code", "Claude Code Proxy":
		return true
	default:
		return false
	}
}

func InitialDisplayNameSource(value string) string {
	if IsGeneratedDisplayName(value) {
		return DisplayNameSourceInitial
	}
	return DisplayNameSourceCustom
}

func CanDeriveDisplayName(value Session, firstUser string) bool {
	switch value.DisplayNameSource {
	case DisplayNameSourceInitial, DisplayNameSourceFirstUser, DisplayNameSourceAITitle:
		return true
	case DisplayNameSourceCustom:
		return false
	}
	if IsGeneratedDisplayName(value.DisplayName) {
		return true
	}
	return firstUser != "" && value.DisplayName == DescribeMessage(firstUser)
}

func ResolveDerivedDisplayName(value Session, values []event.Event) (name, source string, apply bool) {
	name, source = DerivedDisplayName(values)
	if name == "" {
		return "", "", false
	}
	switch value.DisplayNameSource {
	case DisplayNameSourceCustom:
		return "", "", false
	case DisplayNameSourceInitial, DisplayNameSourceFirstUser, DisplayNameSourceAITitle:
		return name, source, true
	}
	if IsGeneratedDisplayName(value.DisplayName) {
		return name, source, true
	}
	for _, item := range values {
		if item.Kind == event.KindUser {
			return name, source, value.DisplayName == DescribeMessage(item.Content)
		}
	}
	return "", "", false
}

func CanApplyFirstUserName(value Session) bool {
	switch value.DisplayNameSource {
	case DisplayNameSourceInitial:
		return true
	case DisplayNameSourceFirstUser, DisplayNameSourceAITitle, DisplayNameSourceCustom:
		return false
	default:
		return IsGeneratedDisplayName(value.DisplayName)
	}
}

func CanApplyAITitle(value Session) bool {
	switch value.DisplayNameSource {
	case DisplayNameSourceInitial, DisplayNameSourceFirstUser, DisplayNameSourceAITitle:
		return true
	case DisplayNameSourceCustom:
		return false
	default:
		return IsGeneratedDisplayName(value.DisplayName)
	}
}

func DerivedDisplayName(values []event.Event) (name, source string) {
	firstUser := ""
	latestTitle := ""
	for _, item := range values {
		if firstUser == "" && item.Kind == event.KindUser {
			firstUser = DescribeMessage(item.Content)
		}
		if item.Kind == event.KindAITitle {
			if title := DescribeMessage(item.Content); title != "" {
				latestTitle = title
			}
		}
	}
	if latestTitle != "" {
		return latestTitle, DisplayNameSourceAITitle
	}
	if firstUser != "" {
		return firstUser, DisplayNameSourceFirstUser
	}
	return "", ""
}

func DescribeMessage(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 96 {
		return value[:96] + "…"
	}
	return value
}
