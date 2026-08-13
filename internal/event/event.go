package event

import "time"

const (
	KindSystem    = "system"
	KindAssistant = "assistant"
	KindUser      = "user"
	KindTool      = "tool"
	KindResult    = "result"
	KindError     = "error"
)

const (
	SourceStream  = "stream"
	SourceHistory = "history"
)

// Event is a normalized observation emitted by an external Agent session.
type Event struct {
	ID         string    `json:"id"`
	ExternalID string    `json:"external_id,omitempty"`
	SessionID  string    `json:"session_id"`
	Source     string    `json:"source,omitempty"`
	Kind       string    `json:"kind"`
	Type       string    `json:"type,omitempty"`
	Role       string    `json:"role,omitempty"`
	Subtype    string    `json:"subtype,omitempty"`
	Content    string    `json:"content"`
	Summary    string    `json:"summary,omitempty"`
	ToolName   string    `json:"tool_name,omitempty"`
	ToolInput  string    `json:"tool_input,omitempty"`
	ToolOutput string    `json:"tool_output,omitempty"`
	Thinking   string    `json:"thinking,omitempty"`
	IsError    bool      `json:"is_error,omitempty"`
	RawJSON    string    `json:"raw_json,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
