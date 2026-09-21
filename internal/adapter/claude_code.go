package adapter

import (
	"context"
	"fmt"

	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
)

type ClaudeCodeAdapter struct {
	Binary string
}

type TurnEvent struct {
	Event event.Event
	Done  bool
}

func NewClaudeCodeAdapter(binary string) *ClaudeCodeAdapter {
	if binary == "" {
		binary = "claude"
	}
	return &ClaudeCodeAdapter{Binary: binary}
}

// Capabilities declares that Agora launches, sends input to, and reads
// history from managed Claude Code sessions (PTY-backed). It does not observe
// externally-started sessions anymore; that was the abandoned import model.
func (a *ClaudeCodeAdapter) Capabilities() Capabilities {
	return Capabilities{CanStart: true, CanDiscover: false, CanSendInput: true, CanStream: true, CanAttach: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanObserve: true}
}

// StartTurn is retained for interface compatibility but is not used: managed
// sessions run continuously in a PTY owned by a Session Host, and input is
// written straight into that PTY.
func (a *ClaudeCodeAdapter) StartTurn(context.Context, session.Session, string, func(TurnEvent)) error {
	return fmt.Errorf("StartTurn is obsolete; managed sessions are driven through the Session Host")
}
