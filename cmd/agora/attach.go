package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/delve8/agora/internal/protocol"
)

// runWrapper is the generic native Agent wrapper entry point. It talks only to
// the local Daemon over a Unix socket. The Daemon owns the Server connection,
// device credential, Agent creation, and session ownership.
func runWrapper(args []string) error {
	var sessionID string
	if len(args) > 0 && looksLikeAgoraSessionID(args[0]) {
		sessionID = strings.TrimSpace(args[0])
		args = args[1:]
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("claude wrapper does not pass Claude options to the Daemon; use the real claude binary for command options")
		}
	}
	var attached protocol.WrapperResponse
	var err error
	if sessionID == "" {
		attached, err = createDaemonWrapperSession("claude", args)
	} else if len(args) > 0 {
		return errors.New("a session id cannot be combined with an initial prompt")
	} else {
		attached, err = attachDaemonWrapperSession(sessionID)
	}
	if err != nil {
		return err
	}
	return runAttachSocket(attached.Socket)
}

func createDaemonWrapperSession(agent string, prompts []string) (protocol.WrapperResponse, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return protocol.WrapperResponse{}, err
	}
	// The workspace is already shown as the parent item in the UI. Keep the
	// wrapper-created session name generated so provider history (Pi session_info
	// or Claude title/first user) can replace it after the history is observed.
	return callLocalDaemon(protocol.WrapperRequest{Workspace: workspace, DisplayName: "New session", Role: "terminal", Agent: agent, Prompts: prompts})
}

func createDaemonWrapperSessionWithArgs(agent string, args []string) (protocol.WrapperResponse, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return protocol.WrapperResponse{}, err
	}
	// The workspace is already shown as the parent item in the UI. Keep the
	// wrapper-created session name generated so provider history (especially Pi
	// session_info) can replace it after the history is observed.
	return callLocalDaemon(protocol.WrapperRequest{Workspace: workspace, DisplayName: "New session", Role: "terminal", Agent: agent, AgentArgs: args})
}

func attachDaemonWrapperSession(sessionID string) (protocol.WrapperResponse, error) {
	return callLocalDaemon(protocol.WrapperRequest{SessionID: sessionID})
}

func callLocalDaemon(payload protocol.WrapperRequest) (protocol.WrapperResponse, error) {
	path := strings.TrimSpace(os.Getenv("AGORA_DAEMON_SOCKET"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return protocol.WrapperResponse{}, fmt.Errorf("resolve home directory for Agora daemon socket: %w", err)
		}
		path = filepath.Join(home, ".agora", "daemon.sock")
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return protocol.WrapperResponse{}, fmt.Errorf("connect to local Agora daemon at %s: %w", path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(payload); err != nil {
		return protocol.WrapperResponse{}, fmt.Errorf("send wrapper request to daemon: %w", err)
	}
	var result protocol.WrapperResponse
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		return protocol.WrapperResponse{}, fmt.Errorf("read wrapper response from daemon: %w", err)
	}
	if result.Error != "" {
		return protocol.WrapperResponse{}, errors.New(result.Error)
	}
	if result.SessionID == "" || result.Socket == "" {
		return protocol.WrapperResponse{}, errors.New("daemon wrapper response is missing session_id or socket")
	}
	return result, nil
}

// runAttach attaches the current terminal to a managed session's PTY through
// the local Daemon control path.
func runAttach(id string) error {
	attached, err := attachDaemonWrapperSession(id)
	if err != nil {
		return err
	}
	return runAttachSocket(attached.Socket)
}

func runAttachSocket(addr string) error {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	done := make(chan error, 2)
	go func() { _, err := io.Copy(os.Stdout, conn); done <- err }()
	go func() { _, err := io.Copy(conn, os.Stdin); done <- err }()
	<-done
	return nil
}
