package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/terminal"
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
		return attachToSession(sessionID, sessionID)
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

// daemonSocketPath is where the local Daemon listens. AGORA_DAEMON_SOCKET
// overrides it, which is how tests and non-default homes reach their daemon.
func daemonSocketPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("AGORA_DAEMON_SOCKET")); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for Agora daemon socket: %w", err)
	}
	return filepath.Join(home, ".agora", "daemon.sock"), nil
}

func callLocalDaemon(payload protocol.WrapperRequest) (protocol.WrapperResponse, error) {
	path, err := daemonSocketPath()
	if err != nil {
		return protocol.WrapperResponse{}, err
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return protocol.WrapperResponse{}, daemonUnavailableError{path: path, err: err}
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

// daemonUnavailableError uses EX_TEMPFAIL so the PATH wrapper can distinguish
// an unreachable local Daemon from validation or Agent errors. Only this error
// permits falling back to the original provider executable.
type daemonUnavailableError struct {
	path string
	err  error
}

func (e daemonUnavailableError) Error() string {
	return fmt.Sprintf("connect to local Agora daemon at %s: %v", e.path, e.err)
}

func (e daemonUnavailableError) Unwrap() error { return e.err }
func (e daemonUnavailableError) ExitCode() int { return 75 }

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

	// The Agent PTY starts at a fixed default so Pi does not exit on 0x0.
	// The wrapper then sends the real window size, including later SIGWINCH
	// updates, as framed attach messages rather than keystrokes.
	var writeMu sync.Mutex
	write := func(fn func() error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return fn()
	}
	sendSize := func() {
		cols, rows, sizeErr := term.GetSize(int(os.Stdin.Fd()))
		if sizeErr != nil {
			return
		}
		_ = write(func() error { return terminal.WriteResize(conn, cols, rows) })
	}
	sendSize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			sendSize()
		}
	}()

	done := make(chan error, 2)
	go func() { _, err := io.Copy(os.Stdout, conn); done <- err }()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				if writeErr := write(func() error { return terminal.WriteData(conn, data) }); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()
	<-done
	return nil
}
