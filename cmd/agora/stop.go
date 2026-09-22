package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/delve8/agora/internal/protocol"
)

// runStopCommand ends a managed session without attaching to it. The Agent is
// asked to exit through its Session Host, so the transcript and runtime
// bookkeeping follow the same path as an in-Agent /quit: the session becomes
// stopped and stays resumable as history.
func runStopCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agora stop <#|session-id>")
	}
	reference := args[0]
	if reference == "" {
		return errors.New("usage: agora stop <#|session-id>")
	}
	entry, err := resolveSessionArgument(reference)
	if err != nil {
		return err
	}
	if !entry.Attachable {
		return fmt.Errorf("session %s is not running, so there is nothing to stop", shortSessionReference(entry))
	}
	if err := stopLocalSession(entry.SessionID); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", describeEntry(entry))
	fmt.Println("the transcript is kept as history; continue it later with `agora wrap " + entry.Agent + "` and /resume")
	return nil
}

func stopLocalSession(sessionID string) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewEncoder(conn).Encode(protocol.SessionStopRequest{Type: protocol.SessionStop, SessionID: sessionID}); err != nil {
		return fmt.Errorf("send stop request to daemon: %w", err)
	}
	var response protocol.SessionStopResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return fmt.Errorf("read stop response from daemon: %w", err)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if !response.Stopped {
		return fmt.Errorf("session %s was not stopped", sessionID)
	}
	return nil
}
