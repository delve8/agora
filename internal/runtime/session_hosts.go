package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/sessionhost"
)

// SessionHostRegistry is the Daemon-side registry of independent per-session
// hosts. It deliberately does not own the host process; closing the Daemon
// only drops these clients.
type SessionHostRegistry struct {
	root       string
	executable string
	mu         sync.Mutex
	clients    map[string]*sessionhost.Client
}

func NewSessionHostRegistry(executable string) *SessionHostRegistry {
	home, _ := os.UserHomeDir()
	return NewSessionHostRegistryWithHome(executable, home)
}

func NewSessionHostRegistryWithHome(executable, home string) *SessionHostRegistry {
	if strings.TrimSpace(executable) == "" {
		executable = os.Args[0]
	}
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	return &SessionHostRegistry{root: filepath.Join(home, ".agora", "runtime", "sessions"), executable: executable, clients: make(map[string]*sessionhost.Client)}
}

func (r *SessionHostRegistry) Spawn(ctx context.Context, value session.Session, command []string, env []string) (*sessionhost.Client, error) {
	if r == nil {
		return nil, errors.New("session-host registry is unavailable")
	}
	hostID := fmt.Sprintf("host-%d", time.Now().UnixNano())
	config := sessionhost.Config{HostID: hostID, SessionID: value.ID, CoordinationID: value.CoordinationID, DaemonID: value.DaemonID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, Workspace: value.Workspace, DisplayName: value.DisplayName, HistoryPath: value.HistoryPath, Command: command, Env: env, RuntimeDir: filepath.Join(r.root, hostID)}
	client, err := sessionhost.SpawnWithEnv(ctx, r.executable, config, env)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.clients[value.ID] = client
	r.mu.Unlock()
	return client, nil
}

func (r *SessionHostRegistry) Put(id string, client *sessionhost.Client) {
	if r == nil || client == nil {
		return
	}
	r.mu.Lock()
	r.clients[id] = client
	r.mu.Unlock()
}

func (r *SessionHostRegistry) Get(id string) (*sessionhost.Client, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.clients[id]
	return value, ok
}

func (r *SessionHostRegistry) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.clients = make(map[string]*sessionhost.Client)
	r.mu.Unlock()
}

func (r *SessionHostRegistry) Delete(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.clients, id)
	r.mu.Unlock()
}

func (r *SessionHostRegistry) Adopt(ctx context.Context) ([]sessionhost.Metadata, error) {
	if r == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(r.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]sessionhost.Metadata, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metadataPath := filepath.Join(r.root, entry.Name(), "metadata.json")
		metadata, loadErr := sessionhost.LoadMetadata(metadataPath)
		if loadErr != nil {
			// An aborted spawn leaves a directory with no usable metadata.
			// Nothing can own it, so it is safe to remove.
			r.discard(entry.Name())
			continue
		}
		client, clientErr := sessionhost.NewClient(metadataPath)
		if clientErr != nil {
			r.discard(entry.Name())
			continue
		}
		if state, stateErr := client.State(ctx); stateErr == nil && state.State == "running" {
			r.Put(metadata.SessionID, client)
			result = append(result, state)
			continue
		}
		// A Host records a terminal state just before it exits, so this runtime
		// state is finished even if the PID is still winding down.
		if hostStateFinished(metadata.State) {
			r.discard(entry.Name())
			continue
		}
		// Otherwise the Host did not answer. Only remove its runtime state when
		// the process is provably gone: a live but unresponsive Host still owns
		// its Agent, and deleting its metadata would orphan that process.
		if hostProcessGone(metadata.HostPID) {
			r.discard(entry.Name())
		}
	}
	return result, nil
}

// discard removes the runtime directory of a Host that no longer exists and any
// sockets it left behind. A Host that is killed cannot clean up after itself.
func (r *SessionHostRegistry) discard(hostID string) {
	if hostID == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(r.root, hostID))
	socketDir := sessionhost.SocketDir()
	for _, suffix := range []string{"-c.sock", "-a.sock"} {
		_ = os.Remove(filepath.Join(socketDir, hostID+suffix))
	}
}

// hostStateFinished reports whether a Host already reached a terminal state.
func hostStateFinished(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "exited", "stopped", "failed":
		return true
	default:
		return false
	}
}

// hostProcessGone reports whether a Host PID is definitely not running. An
// inconclusive check (permission denied, or a reused PID) keeps the runtime
// state, so cleanup can never remove a live Agent.
func hostProcessGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func (r *SessionHostRegistry) Clients() map[string]*sessionhost.Client {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]*sessionhost.Client, len(r.clients))
	for id, client := range r.clients {
		result[id] = client
	}
	return result
}

func normalizeHostAgent(agent string) string { return strings.TrimSpace(strings.ToLower(agent)) }
