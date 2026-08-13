package adapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/delve8/agora/internal/event"
)

type streamMessage struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content"`
}

type streamEnvelope struct {
	Type      string        `json:"type"`
	Subtype   string        `json:"subtype"`
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
	Message   streamMessage `json:"message"`
	Result    string        `json:"result"`
	IsError   bool          `json:"is_error"`
	Stop      string        `json:"stop_reason"`
}

func ParseStreamEvent(sessionID string, data []byte) (TurnEvent, error) {
	var raw streamEnvelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return TurnEvent{}, fmt.Errorf("parse stream event: %w", err)
	}
	content := ""
	for _, block := range raw.Message.Content {
		if block.Text != "" {
			content += block.Text
		}
		if block.Thinking != "" && content == "" {
			content = block.Thinking
		}
	}
	kind := raw.Type
	if raw.Type == "system" && raw.Subtype == "thinking_tokens" {
		return TurnEvent{}, nil
	}
	if raw.Type == "result" && raw.IsError {
		kind = event.KindError
	}
	if raw.Type == "result" && content == "" {
		content = raw.Result
	}
	return TurnEvent{Event: event.Event{ID: newID(), ExternalID: raw.UUID, SessionID: sessionID, Source: event.SourceStream, Kind: kind, Type: streamEventType(raw.Type, kind), Role: streamEventRole(raw.Type), Subtype: raw.Subtype, Content: content, Summary: summarize(content), IsError: raw.IsError || kind == event.KindError, RawJSON: string(data), CreatedAt: now()}, Done: raw.Type == "result"}, nil
}

func streamEventType(rawType, kind string) string {
	switch rawType {
	case "assistant", "message":
		return "text"
	case "tool_use":
		return "tool_call"
	case "tool_result":
		return "tool_result"
	case "result":
		if kind == event.KindError {
			return "error"
		}
		return "status"
	default:
		return kind
	}
}

func streamEventRole(rawType string) string {
	switch rawType {
	case "assistant", "message":
		return "assistant"
	case "user":
		return "user"
	case "tool_use", "tool_result":
		return "tool"
	default:
		return "system"
	}
}

func now() time.Time { return time.Now().UTC() }

func newID() string { return fmt.Sprintf("evt-%d", time.Now().UnixNano()) }
