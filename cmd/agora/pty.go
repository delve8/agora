package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Session registry: maps a session id to the daemon's Unix socket so a later
// `agora attach <id>` can connect and have the daemon relay the PTY. The
// master fd stays inside the daemon process: on macOS its device name is the
// generic /dev/ptmx and cannot be re-opened by another process, so clients
// must go through the daemon (the tmate/ttyd model).
const registryPath = "/tmp/agora-pty-registry.json"
const socketDir = "/tmp/agora-pty"

type ptySession struct {
	ID         string    `json:"id"`
	Command    []string  `json:"command"`
	PID        int       `json:"pid"`
	SocketPath string    `json:"socket_path"`
	StartedAt  time.Time `json:"started_at"`
}

var ptyRegistry struct {
	mu       sync.Mutex
	sessions map[string]ptySession
}

func loadRegistry() {
	ptyRegistry.sessions = make(map[string]ptySession)
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return
	}
	var sessions map[string]ptySession
	if json.Unmarshal(data, &sessions) == nil {
		ptyRegistry.sessions = sessions
	}
}

func saveRegistry() {
	ptyRegistry.mu.Lock()
	defer ptyRegistry.mu.Unlock()
	data, _ := json.MarshalIndent(ptyRegistry.sessions, "", "  ")
	_ = os.WriteFile(registryPath, data, 0o600)
}

// runPTY starts <args> under a PTY owned by Agora and serves it to clients
// over a Unix socket. A client attaches with `agora attach <session-id>`;
// keyboard input from that client is written into the PTY master, and the
// child's output is relayed back. `agora pty claude` launches Claude Code.
func runPTY(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("pty requires a command, e.g. `agora pty claude`")
	}
	command, err := execPath(args[0])
	if err != nil {
		return err
	}
	loadRegistry()
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return err
	}

	// Use a PTY as the child's controlling terminal so Claude Code runs as a
	// full TUI. The master fd lives only in this process.
	cmd := exec.Cmd{Path: command, Args: args}
	file, err := pty.Start(&cmd)
	if err != nil {
		return fmt.Errorf("start PTY: %w", err)
	}
	defer file.Close()

	socketPath := filepath.Join(socketDir, fmt.Sprintf("pty-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer listener.Close()

	session := ptySession{ID: newSessionID(), Command: args, PID: cmd.Process.Pid, SocketPath: socketPath, StartedAt: time.Now().UTC()}
	ptyRegistry.mu.Lock()
	ptyRegistry.sessions[session.ID] = session
	ptyRegistry.mu.Unlock()
	saveRegistry()
	log.Printf("agora: pty session %s running %s (attach: agora attach %s)", session.ID, strings.Join(args, " "), session.ID)

	// Relay child output to the daemon's stdout for a foreground convenience
	// view, and accept attach clients, each relaying both directions.
	go func() {
		_, _ = io.Copy(os.Stdout, file)
	}()
	go acceptClients(listener, file)

	// Exit when the child exits or on SIGINT/SIGTERM.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-signals:
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return nil
}

func acceptClients(listener net.Listener, master *os.File) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			// Client output -> PTY master (input into the child), and child
			// output -> client. Wait on either direction so a client detaching
			// does not hold the session.
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(master, conn)
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(conn, master)
				done <- struct{}{}
			}()
			<-done
		}()
	}
}

func execPath(name string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err != nil {
			return "", fmt.Errorf("command not found: %s", name)
		}
		return absolute, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("command not found: %s", name)
	}
	return path, nil
}

func newSessionID() string {
	return fmt.Sprintf("pty-%d", time.Now().UnixNano())
}
