package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
)

type Config struct {
	ID              string
	Version         string
	ServerURL       string
	Credential      string
	CredentialPath  string
	ClaudeBinary    string
	PiBinary        string
	PiProvider      string
	PiModel         string
	PiSessionDir    string
	LocalSocketPath string
	HomeDir         string
	Heartbeat       time.Duration
	OutboxLimit     int
}

type Daemon struct {
	config           Config
	manager          *runtime.Manager
	localListener    net.Listener
	localSocketPath  string
	connMu           sync.Mutex
	writeMu          sync.Mutex
	resyncMu         sync.Mutex
	conn             *websocket.Conn
	registered       bool
	registeredCh     chan struct{}
	registeredClosed bool
	outbox           *outbox
	eventMu          sync.Mutex
	events           map[string]context.CancelFunc
	historyMu        sync.RWMutex
	historySessions  []session.Session
	// resyncDirty records that the last resync did not reach the Server. History
	// is only advertised when it changes, so without this flag a single lost
	// resync would hide every provider history session until the set changes
	// again. Guarded by resyncMu.
	resyncDirty bool
}

func New(config Config) (*Daemon, error) {
	if config.ID == "" {
		return nil, errors.New("daemon id is required")
	}
	if config.Version == "" {
		config.Version = "dev"
	}
	if config.Heartbeat <= 0 {
		config.Heartbeat = 15 * time.Second
	}
	if config.OutboxLimit <= 0 {
		config.OutboxLimit = 256
	}
	manager := runtime.NewDaemonManager(runtime.NewMemoryStore(), config.ID, adapter.NewClaudeCodeAdapter(config.ClaudeBinary), runtime.NewClaudeProvider(config.ClaudeBinary, config.HomeDir))
	// Session Hosts are the only way an Agent runs: every session gets its own
	// `agora session-host` process, so the Daemon never owns an Agent PTY.
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(runtime.NewPiProvider(runtime.PiConfig{Binary: config.PiBinary, Provider: config.PiProvider, Model: config.PiModel, SessionDir: config.PiSessionDir}))
	daemon := &Daemon{config: config, manager: manager, outbox: newOutbox(config.OutboxLimit), events: make(map[string]context.CancelFunc), registeredCh: make(chan struct{})}
	manager.SetSessionExitHandler(func(value session.Session, exited runtime.AgentExit) {
		daemon.stopEventBridge(value.ID)
		_ = daemon.send(protocol.SessionExit, protocol.ExitPayload{SessionID: value.ID, State: value.State, ExitCode: exited.ExitCode, LastError: value.LastError, Intentional: exited.Intentional})
	})
	manager.SetEventHandler(func(item event.Event) {
		if item.Kind != event.KindResult && !(item.Kind == event.KindError && item.IsError) {
			return
		}
		payload := protocol.EventBatchPayload{SessionID: item.SessionID}
		body, err := json.Marshal([]event.Event{item})
		if err != nil {
			return
		}
		payload.Events = body
		_ = daemon.send(protocol.EventBatch, payload)
	})
	manager.SetSessionUpdateHandler(func(value session.Session) {
		_ = daemon.send(protocol.SessionUpdate, protocol.SessionUpdatePayload{SessionID: value.ID, DaemonID: config.ID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, HistoryPath: value.HistoryPath, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource, State: value.State, Connection: value.Connection, PID: value.ProcessID, LastError: value.LastError})
	})
	manager.SetSessionRebindHandler(func(oldID string, value session.Session) {
		daemon.stopEventBridge(oldID)
		daemon.startEventBridge(value)
		_ = daemon.send(protocol.SessionRebind, protocol.SessionRebindPayload{OldSessionID: oldID, NewSessionID: value.ID, DaemonID: config.ID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, HistoryPath: value.HistoryPath})
		_ = daemon.send(protocol.SessionUpdate, protocol.SessionUpdatePayload{SessionID: value.ID, DaemonID: config.ID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, HistoryPath: value.HistoryPath, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource, State: value.State, Connection: value.Connection, PID: value.ProcessID, LastError: value.LastError})
		_ = daemon.sendResync()
	})
	return daemon, nil
}

func (d *Daemon) startLocalWrapperServer(ctx context.Context) error {
	path := strings.TrimSpace(d.config.LocalSocketPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("AGORA_DAEMON_SOCKET"))
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory for daemon socket: %w", err)
		}
		path = filepath.Join(home, ".agora", "daemon.sock")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("daemon wrapper socket path exists and is not a Unix socket: %s", path)
		}
		if existing, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond); dialErr == nil {
			_ = existing.Close()
			return fmt.Errorf("daemon wrapper socket is already in use: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale daemon wrapper socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect daemon wrapper socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on daemon wrapper socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return err
	}
	d.localListener = listener
	d.localSocketPath = path
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("agora daemon: local wrapper socket stopped: %v", err)
				}
				return
			}
			go d.handleLocalWrapper(conn)
		}
	}()
	log.Printf("agora daemon: local wrapper socket at %s", path)
	return nil
}

func (d *Daemon) handleLocalWrapper(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	decoder := json.NewDecoder(io.LimitReader(conn, 1<<20))
	// The local socket carries both wrapper requests and provider session
	// reports from the injected Agent extension. Decode the body once and branch
	// on the message type so neither shape has to know about the other.
	var body json.RawMessage
	if err := decoder.Decode(&body); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: "invalid wrapper request: " + err.Error()})
		return
	}
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &envelope)
	switch envelope.Type {
	case protocol.SessionReport:
		d.handleSessionReport(conn, body)
		return
	case protocol.SessionList:
		d.handleSessionList(conn, body)
		return
	case protocol.SessionSnapshot:
		d.handleSessionSnapshot(conn, body)
		return
	}
	var payload protocol.WrapperRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: "invalid wrapper request: " + err.Error()})
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: err.Error()})
		return
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	if err := d.waitRegistered(readyCtx); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: err.Error()})
		return
	}
	endpoint, err := daemonHTTPBase(d.config.ServerURL)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: err.Error()})
		return
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/api/daemon/wrap", bytes.NewReader(body))
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: err.Error()})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Agora-Daemon-ID", d.config.ID)
	if d.config.Credential != "" {
		request.Header.Set("Authorization", "Bearer "+d.config.Credential)
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	request = request.WithContext(requestCtx)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.WrapperResponse{Error: "request Server through daemon: " + err.Error()})
		return
	}
	defer response.Body.Close()
	var result protocol.WrapperResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		result.Error = err.Error()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if result.Error == "" {
			result.Error = response.Status
		}
	}
	_ = json.NewEncoder(conn).Encode(result)
}

// handleSessionReport applies a provider-native session report. The injected
// Agent extension sends it after the provider switches session, which is the
// only exact signal available: a switch writes nothing to any transcript.
func (d *Daemon) handleSessionReport(conn net.Conn, body []byte) {
	var payload protocol.SessionReportPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionReportResponse{Error: err.Error()})
		return
	}
	if strings.TrimSpace(payload.HostID) == "" {
		_ = json.NewEncoder(conn).Encode(protocol.SessionReportResponse{Error: "host_id is required"})
		return
	}
	if d.manager == nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionReportResponse{Error: "session manager is unavailable"})
		return
	}
	value, err := d.manager.ReportAgentSession(context.Background(), runtime.AgentSessionReport{
		HostID:              payload.HostID,
		Reason:              payload.Reason,
		SessionFile:         payload.SessionFile,
		TargetSessionFile:   payload.TargetSessionFile,
		PreviousSessionFile: payload.PreviousSessionFile,
		NativeSessionID:     payload.SessionID,
		SessionName:         payload.SessionName,
	})
	if err != nil {
		log.Printf("agora daemon: session report for host %s was not applied: %v", payload.HostID, err)
		_ = json.NewEncoder(conn).Encode(protocol.SessionReportResponse{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(conn).Encode(protocol.SessionReportResponse{Applied: true, SessionID: value.ID})
}

func (d *Daemon) waitRegistered(ctx context.Context) error {
	for {
		d.connMu.Lock()
		if d.registered {
			d.connMu.Unlock()
			return nil
		}
		if d.registeredCh == nil {
			d.registeredCh = make(chan struct{})
		}
		ready := d.registeredCh
		d.connMu.Unlock()
		select {
		case <-ready:
			// A disconnect also wakes waiters. Re-check registered so a stale
			// connection cannot make a wrapper request race the reconnect.
			continue
		case <-ctx.Done():
			return fmt.Errorf("daemon is not connected to the Server: %w", ctx.Err())
		}
	}
}

func daemonHTTPBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "ws://") {
		raw = "http://" + strings.TrimPrefix(raw, "ws://")
	} else if strings.HasPrefix(raw, "wss://") {
		raw = "https://" + strings.TrimPrefix(raw, "wss://")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid daemon server URL %q", raw)
	}
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/api/daemon/ws")
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (d *Daemon) Close() error {
	if d.localListener != nil {
		_ = d.localListener.Close()
	}
	if d.localSocketPath != "" {
		_ = os.Remove(d.localSocketPath)
	}
	d.disconnect()
	d.eventMu.Lock()
	for id, cancel := range d.events {
		cancel()
		delete(d.events, id)
	}
	d.eventMu.Unlock()
	if d.manager != nil {
		d.manager.Close()
	}
	return nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if d.config.ServerURL == "" {
		return errors.New("daemon server URL is required")
	}
	if err := d.manager.AdoptSessionHosts(ctx); err != nil {
		return err
	}
	if err := d.manager.ReconcileObservers(ctx); err != nil {
		return err
	}
	if err := d.manager.ResumeManagedSessions(ctx); err != nil {
		return err
	}
	if err := d.startLocalWrapperServer(ctx); err != nil {
		return err
	}
	values, err := d.manager.ListSessions(context.Background(), "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Source == session.SourceManaged {
			d.startEventBridge(value)
		}
	}
	go d.discoverHistory(ctx)
	var retryAttempt int
	for {
		if err := d.connect(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := reconnectDelay(retryAttempt)
			retryAttempt++
			log.Printf("agora daemon: connect to %s failed: %v; retrying in %s", d.config.ServerURL, err, delay)
			if err := waitReconnect(ctx, delay); err != nil {
				return err
			}
			continue
		}
		retryAttempt = 0
		log.Printf("agora daemon: connected to %s", d.config.ServerURL)
		if err := d.session(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := reconnectDelay(retryAttempt)
			retryAttempt++
			log.Printf("agora daemon: disconnected: %v; reconnecting in %s", err, delay)
			if err := waitReconnect(ctx, delay); err != nil {
				return err
			}
		}
	}
}

const historyDiscoveryInterval = 2 * time.Second

// discoverHistory keeps the daemon's provider history index up to date. A
// daemon can start before an external Pi process creates its JSONL file, so a
// one-shot scan at startup is not sufficient for sessions that were not
// created through the Agora wrapper.
func (d *Daemon) discoverHistory(ctx context.Context) {
	ticker := time.NewTicker(historyDiscoveryInterval)
	defer ticker.Stop()

	d.refreshHistory(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshHistory(ctx, false)
		}
	}
}

func (d *Daemon) refreshHistory(ctx context.Context, forceResync bool) {
	values, err := d.manager.DiscoverHistorySessions(ctx, "", d.config.ID)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("agora daemon: history discovery failed: %v", err)
		}
		return
	}

	d.historyMu.Lock()
	changed := !sameHistorySessions(d.historySessions, values)
	d.historySessions = values
	d.historyMu.Unlock()
	d.resyncMu.Lock()
	dirty := d.resyncDirty
	d.resyncMu.Unlock()
	if !shouldResyncHistory(changed, forceResync, dirty) {
		return
	}

	log.Printf("agora daemon: indexed %d history sessions", len(values))
	if err := d.sendResync(); err != nil && ctx.Err() == nil {
		log.Printf("agora daemon: history resync deferred: %v", err)
	}
}

func sameHistorySessions(left, right []session.Session) bool {
	if len(left) != len(right) {
		return false
	}
	byKey := func(values []session.Session) map[string]session.Session {
		result := make(map[string]session.Session, len(values))
		for _, value := range values {
			key := value.Agent + "\x00" + value.AgentSessionID
			if value.AgentSessionID == "" {
				key = value.ID
			}
			result[key] = value
		}
		return result
	}
	leftByKey, rightByKey := byKey(left), byKey(right)
	for key, current := range leftByKey {
		updated, exists := rightByKey[key]
		if !exists || current.ID != updated.ID || current.Agent != updated.Agent ||
			current.AgentSessionID != updated.AgentSessionID || current.HistoryPath != updated.HistoryPath ||
			current.Workspace != updated.Workspace || current.DisplayName != updated.DisplayName ||
			current.DisplayNameSource != updated.DisplayNameSource || current.Role != updated.Role ||
			!current.CreatedAt.Equal(updated.CreatedAt) || !current.UpdatedAt.Equal(updated.UpdatedAt) {
			return false
		}
	}
	return true
}

func reconnectDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 5 {
		attempt = 5
	}
	return time.Duration(1<<attempt) * time.Second
}

func waitReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *Daemon) connect(ctx context.Context) error {
	header := http.Header{}
	if d.config.Credential != "" {
		header.Set("Authorization", "Bearer "+d.config.Credential)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, d.config.ServerURL, header)
	if err != nil {
		return err
	}
	conn.SetReadLimit(protocol.MaxFrameSize)
	d.connMu.Lock()
	d.conn = conn
	d.registered = false
	d.registeredCh = make(chan struct{})
	d.registeredClosed = false
	d.connMu.Unlock()
	return nil
}

func (d *Daemon) session(ctx context.Context) error {
	defer d.disconnect()
	if err := d.send(protocol.DaemonRegister, protocol.DaemonRegisterPayload{DaemonID: d.config.ID, Version: d.config.Version, Hostname: hostname(), Capabilities: map[string]bool{"can_start": true, "can_attach": true, "can_observe": true, "can_send_input": true, "can_stream": true, "can_interrupt": true, "can_resume": true, "can_approve": false, "can_read_history": true, "can_read_terminal": true}}); err != nil {
		return err
	}
	log.Printf("agora daemon: registered as %s", d.config.ID)
	if err := d.flushOutbox(); err != nil {
		return err
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(d.config.Heartbeat)
		defer ticker.Stop()
		heartbeats := 0
		for {
			select {
			case <-heartbeatCtx.Done():
				d.connMu.Lock()
				if d.conn != nil {
					_ = d.conn.Close()
				}
				d.connMu.Unlock()
				heartbeatDone <- nil
				return
			case <-ticker.C:
				if err := d.send(protocol.DaemonHeartbeat, protocol.HeartbeatPayload{DaemonID: d.config.ID, At: time.Now().UTC()}); err != nil {
					d.disconnect()
					heartbeatDone <- err
					return
				}
				heartbeats++
				if heartbeats%4 == 0 {
					if err := d.sendResync(); err != nil {
						d.disconnect()
						heartbeatDone <- err
						return
					}
				}
			}
		}
	}()
	for {
		d.connMu.Lock()
		conn := d.conn
		d.connMu.Unlock()
		if conn == nil {
			return errors.New("daemon connection is closed")
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var frame protocol.Envelope
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if err := d.handle(frame); err != nil {
			return err
		}
		select {
		case err := <-heartbeatDone:
			if err != nil {
				return err
			}
		default:
		}
	}
}

func (d *Daemon) handle(frame protocol.Envelope) error {
	switch frame.Type {
	case protocol.DaemonRegistered:
		d.connMu.Lock()
		if d.registeredCh == nil {
			d.registeredCh = make(chan struct{})
			d.registeredClosed = false
		}
		if !d.registered {
			d.registered = true
			if !d.registeredClosed {
				close(d.registeredCh)
				d.registeredClosed = true
			}
		}
		d.connMu.Unlock()
		return d.sendResync()
	case protocol.ServerResyncRequest:
		return d.sendResync()
	case protocol.DaemonHeartbeatAck:
		return nil
	case protocol.Error:
		// The Server rejects a frame by replying with an error envelope. That is
		// a response to one request, not a connection failure: treating it as
		// unsupported (and therefore fatal) would cycle the Daemon connection
		// whenever a frame is refused, for example a rebind whose target id
		// already exists.
		var payload protocol.ErrorPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		log.Printf("agora daemon: server rejected %s: %s (%s)", frame.RequestID, payload.Message, payload.Code)
		return nil
	case protocol.Ack:
		var payload protocol.AckPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		d.outbox.Remove(payload.AckMessageID)
		return nil
	case protocol.SessionCreate:
		var payload protocol.SessionCreatePayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		var value session.Session
		var err error
		agent := strings.ToLower(strings.TrimSpace(payload.Agent))
		if agent == "" {
			agent = "claude"
		}
		if agent == "claude-code" {
			agent = "claude"
		}
		if agent != "claude" && agent != "pi" {
			err = fmt.Errorf("unsupported agent %q", agent)
		} else if payload.DaemonID != "" && payload.DaemonID != d.config.ID {
			// The server routes create frames to the targeted daemon's
			// connection, but the daemon verifies the declared target anyway so
			// a misrouted frame can never start work on the wrong device.
			err = fmt.Errorf("session create target %s does not match this daemon %s", payload.DaemonID, d.config.ID)
		} else if payload.ResumeID != "" {
			identity, identityErr := session.ParseSessionID(payload.SessionID)
			if identityErr != nil {
				err = identityErr
			} else if identity.DaemonID != d.config.ID || identity.Agent != agent {
				err = fmt.Errorf("session %s is not owned by daemon %s", payload.SessionID, d.config.ID)
			} else {
				workspace := strings.TrimSpace(payload.Workspace)
				if workspace == "" {
					workspace = d.resolveWorkspace(payload.SessionID)
				}
				if workspace == "" {
					err = fmt.Errorf("session %s workspace not found in daemon", payload.SessionID)
				} else {
					value = session.Session{ID: payload.SessionID, CoordinationID: payload.CoordinationID, DaemonID: identity.DaemonID, Agent: identity.Agent, AgentSessionID: identity.AgentSessionID, Workspace: workspace, HistoryPath: payload.HistoryPath, DisplayName: payload.DisplayName, Role: payload.Role, State: session.StateStarting, Source: session.SourceManaged, Capabilities: session.Capabilities{CanStart: true, CanResume: true, CanReadHistory: true}}
					if agent == "claude" {
						value.ClaudeSessionID = strings.TrimPrefix(identity.AgentSessionID, "claude://")
					}
					value, err = d.manager.ResumeSession(runtime.WithTerminalEnv(context.Background(), payload.Terminal), value)
				}
			}
		} else {
			workspace := strings.TrimSpace(payload.Workspace)
			if workspace == "" {
				err = errors.New("workspace is required")
			} else {
				workspace, err = filepath.Abs(workspace)
				if err == nil {
					if info, statErr := os.Stat(workspace); statErr != nil || !info.IsDir() {
						err = errors.New("workspace must be an existing directory")
					}
				}
			}
			provisional := "pending/" + protocol.NewID("session")
			if err == nil {
				// The requesting terminal's identity travels with the create request:
				// the Daemon is a service, so it has no TERM of its own to give the
				// Agent, and an Agent without one renders for 16 colours.
				createCtx := runtime.WithTerminalEnv(context.Background(), payload.Terminal)
				value, err = d.manager.CreateManagedSessionWithAgentArgs(createCtx, provisional, payload.CoordinationID, workspace, payload.DisplayName, payload.Role, agent, payload.AgentArgs)
			}
			if err == nil {
				// A Pi invocation that forwards the user's arguments cannot be told
				// which session id to use, so the provider may create its own and
				// report it before the identity is adopted here. Adopt what the
				// provider already decided instead of overwriting it.
				if resolved := d.manager.ResolveSessionID(provisional); resolved != provisional {
					value, err = d.manager.GetSession(context.Background(), resolved)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					nativeID, waitErr := d.manager.WaitAgentSessionID(ctx, provisional)
					cancel()
					if waitErr != nil {
						err = waitErr
					} else {
						uri := agent + "://" + nativeID
						canonicalID, identityErr := session.NewSessionID(d.config.ID, agent, uri)
						if identityErr != nil {
							err = identityErr
						} else if value, err = d.manager.SetAgentIdentity(context.Background(), provisional, agent, uri); err == nil {
							value, err = d.manager.RekeySession(context.Background(), provisional, canonicalID)
						}
					}
				}
			}
		}
		created := protocol.SessionCreatedPayload{SessionID: value.ID, DaemonID: d.config.ID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, Workspace: value.Workspace}
		if err != nil {
			created.Error = err.Error()
			return d.sendResponse(protocol.SessionCreated, created, frame.RequestID)
		}
		created.PID = value.ProcessID
		created.ClaudeSessionID = value.ClaudeSessionID
		created.HistoryPath = value.HistoryPath
		created.Capabilities = capabilitiesMap(value.Capabilities)
		d.startEventBridge(value)
		if err := d.sendResponse(protocol.SessionCreated, created, frame.RequestID); err != nil {
			return err
		}
		if err := d.send(protocol.SessionUpdate, protocol.SessionUpdatePayload{SessionID: value.ID, DaemonID: d.config.ID, Agent: value.Agent, AgentSessionID: value.AgentSessionID, HistoryPath: value.HistoryPath, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource, ClaudeSessionID: value.ClaudeSessionID, State: value.State, Connection: value.Connection, PID: value.ProcessID, LastError: value.LastError}); err != nil {
			return err
		}
		// Session creation can race the catalog poll: the same Pi JSONL may have
		// already been placed in the history bucket before the live canonical ID
		// was known. Reconcile both buckets immediately after rekeying.
		return d.sendResync()
	case protocol.SessionInput:
		var payload protocol.InputPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		value, valueErr := d.manager.GetSession(context.Background(), payload.SessionID)
		result := protocol.InputResultPayload{SessionID: payload.SessionID}
		if valueErr != nil {
			result.Error = fmt.Sprintf("session %s not found", payload.SessionID)
		} else if err := d.manager.Send(context.Background(), value, messageForInput(payload.Content)); err != nil {
			result.Error = err.Error()
		} else {
			result.Accepted = true
		}
		return d.sendResponse(protocol.SessionInputResult, result, frame.RequestID)
	case protocol.SessionStop:
		var payload protocol.StopPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		value, valueErr := d.manager.GetSession(context.Background(), payload.SessionID)
		result := protocol.StopResultPayload{SessionID: payload.SessionID}
		if valueErr != nil {
			result.Error = fmt.Sprintf("session %s not found", payload.SessionID)
		} else if err := d.manager.StopSession(value); err != nil {
			result.Error = err.Error()
		} else {
			result.Accepted = true
		}
		return d.sendResponse(protocol.SessionStopResult, result, frame.RequestID)
	case protocol.SessionDelete:
		var payload protocol.DeletePayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		result := protocol.DeleteResultPayload{SessionID: payload.SessionID}
		value, valueErr := d.manager.GetSession(context.Background(), payload.SessionID)
		if valueErr != nil || value.ID == "" {
			// A history-only session may never have been persisted in the
			// manager store; its transcript is still deletable.
			d.historyMu.RLock()
			for _, discovered := range d.historySessions {
				if discovered.ID == payload.SessionID {
					value = discovered
					valueErr = nil
					break
				}
			}
			d.historyMu.RUnlock()
		}
		if valueErr != nil || value.ID == "" {
			result.Error = fmt.Sprintf("session %s not found", payload.SessionID)
		} else if d.sessionRunning(value) {
			result.Error = "session is running; stop it before deleting"
		} else if err := d.manager.DeleteSession(context.Background(), value); err != nil {
			result.Error = err.Error()
		} else {
			result.Deleted = true
		}
		if err := d.sendResponse(protocol.SessionDeleteResult, result, frame.RequestID); err != nil {
			return err
		}
		if !result.Deleted {
			return nil
		}
		// Drop the history entry from the cache before resyncing: localSessions
		// reads this slice, and the file is already gone from disk.
		d.historyMu.Lock()
		if len(d.historySessions) > 0 {
			filtered := d.historySessions[:0]
			for _, item := range d.historySessions {
				if item.ID != payload.SessionID {
					filtered = append(filtered, item)
				}
			}
			d.historySessions = filtered
		}
		d.historyMu.Unlock()
		d.stopEventBridge(payload.SessionID)
		return d.sendResync()
	case protocol.SessionHistoryRequest:
		log.Printf("agora daemon: history request for %s", frame.RequestID)
		var payload protocol.HistoryRequestPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		response := protocol.HistoryResponsePayload{SessionID: payload.SessionID}
		value, valueErr := d.manager.GetSession(context.Background(), payload.SessionID)
		if valueErr != nil {
			d.historyMu.RLock()
			historySessions := append([]session.Session(nil), d.historySessions...)
			d.historyMu.RUnlock()
			for _, discovered := range historySessions {
				if discovered.ID == payload.SessionID {
					value = discovered
					valueErr = nil
					break
				}
			}
		}
		if valueErr != nil {
			// The background catalog scan intentionally runs asynchronously so a
			// daemon can connect quickly. A page may request history in that small
			// window, so resolve this one session synchronously instead of exposing
			// a transient "not found" response to the UI.
			if discovered, found, discoverErr := d.manager.HistorySession(context.Background(), payload.SessionID, "", d.config.ID); discoverErr == nil && found {
				value = discovered
				valueErr = nil
			}
		}
		var values []event.Event
		if valueErr != nil {
			valueErr = fmt.Errorf("history session %s not found", payload.SessionID)
		} else {
			values, valueErr = d.manager.HistoryForSession(context.Background(), value, 0)
			if valueErr == nil && payload.Before != "" {
				for index := range values {
					if values[index].ID == payload.Before {
						values = values[:index]
						break
					}
				}
			}
			if valueErr == nil && payload.Limit > 0 && len(values) > payload.Limit {
				values = values[len(values)-payload.Limit:]
			}
		}
		if valueErr != nil {
			response.Error = valueErr.Error()
		} else if body, marshalErr := json.Marshal(values); marshalErr != nil {
			response.Error = marshalErr.Error()
		} else {
			response.Events = body
		}
		log.Printf("agora daemon: history response %s events=%d error=%q", frame.RequestID, len(values), response.Error)
		return d.sendResponse(protocol.SessionHistoryResponse, response, frame.RequestID)
	case protocol.AttachRequest:
		var payload protocol.AttachPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		response := protocol.AttachPayload{SessionID: payload.SessionID}
		if value, err := d.manager.GetSession(context.Background(), payload.SessionID); err != nil {
			response.Error = err.Error()
		} else {
			// The provider may have rekeyed the session after the caller was
			// handed its id (a Pi process that picks its own session). Attach the
			// session the id now names, not the stale string.
			target := value.ID
			var socket string
			socket, err = d.manager.AttachAddr(target)
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Socket = socket
			}
		}
		return d.sendResponse(protocol.AttachResponse, response, frame.RequestID)
	case protocol.SnapshotRequest:
		var payload protocol.SnapshotPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		response := protocol.SnapshotPayload{SessionID: payload.SessionID}
		if snapshot, err := d.manager.Snapshot(payload.SessionID); err != nil {
			response.Error = err.Error()
		} else if body, err := json.Marshal(snapshot); err != nil {
			response.Error = err.Error()
		} else {
			response.Snapshot = body
		}
		return d.sendResponse(protocol.SnapshotResponse, response, frame.RequestID)
	default:
		return fmt.Errorf("unsupported server message %q", frame.Type)
	}
}

func (d *Daemon) resolveWorkspace(sessionID string) string {
	if value, err := d.manager.GetSession(context.Background(), sessionID); err == nil && strings.TrimSpace(value.Workspace) != "" {
		return value.Workspace
	}
	d.historyMu.RLock()
	historySessions := append([]session.Session(nil), d.historySessions...)
	d.historyMu.RUnlock()
	for _, value := range historySessions {
		if value.ID == sessionID {
			return value.Workspace
		}
	}
	return ""
}

// localSessions returns what this Daemon owns and what it has discovered, with
// every history entry that a live session already covers removed. Resync and the
// terminal-facing session list share it so both describe the same set.
func (d *Daemon) localSessions(ctx context.Context) ([]session.Session, []session.Session, error) {
	values, err := d.manager.ListSessions(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	managed := make([]session.Session, 0, len(values))
	managedAgentSessionIDs := make(map[string]struct{})
	managedHistoryPaths := make(map[string]struct{})
	for _, value := range values {
		if value.Source != session.SourceManaged || strings.HasPrefix(value.ID, "pending/") {
			// `pending/...` is an internal creation key. It must never be
			// advertised to Server because the provider catalog may already have
			// advertised the same native session under its canonical URI.
			continue
		}
		// A managed session can be rekeyed before its explicit metadata update
		// reaches every in-memory view. Recover the native URI from the canonical
		// Agora ID so its catalog row is filtered out immediately.
		if value.AgentSessionID == "" {
			value.AgentSessionID = value.NativeSessionURI()
		}
		if value.AgentSessionID != "" {
			managedAgentSessionIDs[value.AgentSessionID] = struct{}{}
		}
		if value.HistoryPath != "" {
			managedHistoryPaths[value.Agent+"\x00"+filepath.Clean(value.HistoryPath)] = struct{}{}
		}
		managed = append(managed, value)
	}
	d.historyMu.RLock()
	discovered := append([]session.Session(nil), d.historySessions...)
	d.historyMu.RUnlock()
	history := make([]session.Session, 0, len(discovered))
	for _, value := range discovered {
		uri := value.NativeSessionURI()
		if _, exists := managedAgentSessionIDs[uri]; exists {
			continue
		}
		if value.HistoryPath != "" {
			if _, exists := managedHistoryPaths[value.Agent+"\x00"+filepath.Clean(value.HistoryPath)]; exists {
				continue
			}
		}
		history = append(history, value)
	}
	return managed, history, nil
}

// handleSessionList answers a local terminal asking which sessions exist, so the
// wrapper does not have to be handed a canonical id. It never contacts the
// Server: this is a local question about the machine the terminal is on.
func (d *Daemon) handleSessionList(conn net.Conn, body []byte) {
	var payload protocol.SessionListRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionListResponse{Error: "invalid session list request: " + err.Error()})
		return
	}
	managed, history, err := d.localSessions(context.Background())
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionListResponse{Error: err.Error()})
		return
	}
	want := normaliseWorkspace(payload.Workspace)
	entries := make([]protocol.SessionListEntry, 0, len(managed)+len(history))
	for _, value := range managed {
		if want != "" && normaliseWorkspace(value.Workspace) != want {
			continue
		}
		entries = append(entries, protocol.SessionListEntry{
			SessionID: value.ID, AgentSessionID: value.NativeSessionURI(), Agent: value.Agent,
			DisplayName: value.DisplayName, Workspace: value.Workspace, State: value.State, Source: session.SourceManaged,
			Attachable: value.State == session.StateRunning || value.State == session.StateStarting || value.State == session.StateWaiting,
			UpdatedAt:  value.UpdatedAt,
		})
	}
	for _, value := range history {
		if want != "" && normaliseWorkspace(value.Workspace) != want {
			continue
		}
		entries = append(entries, protocol.SessionListEntry{
			SessionID: value.ID, AgentSessionID: value.NativeSessionURI(), Agent: value.Agent,
			DisplayName: value.DisplayName, Workspace: value.Workspace, State: value.State, Source: session.SourceHistory,
			UpdatedAt: value.UpdatedAt,
		})
	}
	_ = json.NewEncoder(conn).Encode(protocol.SessionListResponse{Sessions: entries})
}

// handleSessionSnapshot answers a local terminal asking what a session is
// showing. The screen is read through the session host's control socket, which
// has served snapshots since the first host release, so a host built before
// attach-time replay still hands its screen over on request.
func (d *Daemon) handleSessionSnapshot(conn net.Conn, body []byte) {
	var payload protocol.SessionSnapshotRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: "invalid session snapshot request: " + err.Error()})
		return
	}
	if strings.TrimSpace(payload.SessionID) == "" {
		_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: "session_id is required"})
		return
	}
	if d.manager == nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: "session manager is unavailable"})
		return
	}
	snapshot, err := d.manager.Snapshot(payload.SessionID)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: err.Error()})
		return
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Snapshot: encoded})
}

// normaliseWorkspace compares directories the way the rest of Agora does, and
// resolves symlinks so a session started through /var and a terminal in
// /private/var (macOS) still match.
func normaliseWorkspace(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func (d *Daemon) sendResync() error {
	d.resyncMu.Lock()
	defer d.resyncMu.Unlock()

	managed, historySessions, err := d.localSessions(context.Background())
	if err != nil {
		return err
	}
	payload := protocol.ResyncPayload{DaemonID: d.config.ID, Gap: d.outbox.Gap()}
	for _, value := range managed {
		payload.Sessions = append(payload.Sessions, protocol.SessionSummary{
			SessionID: value.ID, DaemonID: d.config.ID, Agent: value.Agent,
			AgentSessionID: value.AgentSessionID, HistoryPath: value.HistoryPath, ClaudeSessionID: value.ClaudeSessionID,
			Workspace: value.Workspace, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource,
			Role:  value.Role,
			State: value.State, Connection: value.Connection, PID: value.ProcessID,
			CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		})
	}
	for _, value := range historySessions {
		payload.History = append(payload.History, protocol.HistorySessionSummary{SessionID: value.ID, DaemonID: d.config.ID, Agent: value.Agent, AgentSessionID: value.NativeSessionURI(), HistoryPath: value.HistoryPath, ClaudeSessionID: value.ClaudeSessionID, Workspace: value.Workspace, DisplayName: value.DisplayName, DisplayNameSource: value.DisplayNameSource, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt})
	}
	parts := splitResync(payload)
	for index := range parts {
		parts[index].Part = index
		parts[index].Chunked = len(parts) > 1
		parts[index].Final = index == len(parts)-1
		if err := d.send(protocol.DaemonResync, parts[index]); err != nil {
			// The Server may not have received the history set; make the next
			// discovery cycle send it again even when nothing changed.
			d.resyncDirty = true
			return err
		}
	}
	d.resyncDirty = false
	return nil
}

const resyncPartBudget = 512 * 1024

// shouldResyncHistory reports whether a discovery cycle must advertise the
// history set: it changed, the caller forces it, or the previous attempt did not
// reach the Server.
func shouldResyncHistory(changed, forced, dirty bool) bool {
	return changed || forced || dirty
}

func splitResync(payload protocol.ResyncPayload) []protocol.ResyncPayload {
	all := make([]protocol.ResyncPayload, 0, len(payload.Sessions)+len(payload.History))
	for _, value := range payload.Sessions {
		all = append(all, protocol.ResyncPayload{DaemonID: payload.DaemonID, Sessions: []protocol.SessionSummary{value}, Gap: payload.Gap})
	}
	for _, value := range payload.History {
		all = append(all, protocol.ResyncPayload{DaemonID: payload.DaemonID, History: []protocol.HistorySessionSummary{value}, Gap: payload.Gap})
	}
	if len(all) == 0 {
		return []protocol.ResyncPayload{{DaemonID: payload.DaemonID, Gap: payload.Gap}}
	}
	parts := make([]protocol.ResyncPayload, 0)
	current := protocol.ResyncPayload{DaemonID: payload.DaemonID, Gap: payload.Gap}
	for _, item := range all {
		candidate := current
		candidate.Sessions = append(append([]protocol.SessionSummary(nil), current.Sessions...), item.Sessions...)
		candidate.History = append(append([]protocol.HistorySessionSummary(nil), current.History...), item.History...)
		body, _ := json.Marshal(candidate)
		if len(body) > resyncPartBudget && (len(current.Sessions) > 0 || len(current.History) > 0) {
			parts = append(parts, current)
			current = item
			continue
		}
		current = candidate
	}
	return append(parts, current)
}

func capabilitiesMap(value session.Capabilities) map[string]bool {
	return map[string]bool{"can_start": value.CanStart, "can_discover": value.CanDiscover, "can_attach": value.CanAttach, "can_observe": value.CanObserve, "can_send_input": value.CanSendInput, "can_stream": value.CanStream, "can_interrupt": value.CanInterrupt, "can_resume": value.CanResume, "can_approve": value.CanApprove, "can_read_history": value.CanReadHistory, "can_read_terminal": value.CanReadTerminal}
}

// sessionRunning reports whether a session currently owns a live Agent
// process. The Host registry is authoritative; the stored row only covers the
// case where the Host is gone but the last state was still running.
func (d *Daemon) sessionRunning(value session.Session) bool {
	if d.manager.IsRunning(value.ID) {
		return true
	}
	switch value.State {
	case session.StateRunning, session.StateWaiting, session.StateStarting:
		return value.ProcessID > 0
	default:
		return false
	}
}

func (d *Daemon) stopEventBridge(id string) {
	d.eventMu.Lock()
	if cancel := d.events[id]; cancel != nil {
		cancel()
		delete(d.events, id)
	}
	d.eventMu.Unlock()
}

func (d *Daemon) startEventBridge(value session.Session) {
	d.eventMu.Lock()
	if _, exists := d.events[value.ID]; exists {
		d.eventMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.events[value.ID] = cancel
	d.eventMu.Unlock()

	ch, unsubscribe := d.manager.Subscribe(value.CoordinationID)
	go func() {
		defer unsubscribe()
		defer func() {
			d.eventMu.Lock()
			delete(d.events, value.ID)
			d.eventMu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case item, ok := <-ch:
				if !ok {
					return
				}
				if item.SessionID != value.ID {
					continue
				}
				payload := protocol.EventBatchPayload{SessionID: value.ID}
				body, err := json.Marshal([]event.Event{item})
				if err != nil {
					continue
				}
				payload.Events = body
				if err := d.send(protocol.EventBatch, payload); err != nil {
					return
				}
			}
		}
	}()
}

func (d *Daemon) sendResponse(typ string, payload any, requestID string) error {
	frame, err := protocol.NewEnvelope(typ, payload)
	if err != nil {
		return err
	}
	frame.RequestID = requestID
	return d.sendFrame(frame)
}

func (d *Daemon) send(typ string, payload any) error {
	frame, err := protocol.NewEnvelope(typ, payload)
	if err != nil {
		return err
	}
	return d.sendFrame(frame)
}

func (d *Daemon) sendFrame(frame protocol.Envelope) error {
	d.connMu.Lock()
	conn := d.conn
	d.connMu.Unlock()
	if conn == nil {
		d.outbox.Add(frame)
		return nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if err := conn.WriteJSON(frame); err != nil {
		d.disconnect()
		d.outbox.Add(frame)
		return err
	}
	return nil
}

func (d *Daemon) flushOutbox() error {
	for _, frame := range d.outbox.Items() {
		if err := d.sendDirect(frame); err != nil {
			return err
		}
		d.outbox.Remove(frame.MessageID)
	}
	return nil
}

func (d *Daemon) sendDirect(frame protocol.Envelope) error {
	d.connMu.Lock()
	conn := d.conn
	d.connMu.Unlock()
	if conn == nil {
		return errors.New("daemon connection is closed")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return conn.WriteJSON(frame)
}

func (d *Daemon) disconnect() {
	d.connMu.Lock()
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	if d.registeredCh != nil && !d.registeredClosed {
		// Wake local wrapper requests waiting on a connection that just died;
		// waitRegistered will re-check the channel after the next connect.
		close(d.registeredCh)
		d.registeredClosed = true
	}
	d.registered = false
	d.registeredCh = make(chan struct{})
	d.registeredClosed = false
	d.connMu.Unlock()
}

func hostname() string {
	value, _ := os.Hostname()
	return value
}

func messageForInput(content string) message.Message {
	return message.Message{ID: protocol.NewID("msg"), Content: content}
}

func credentialPath(path string) string {
	if path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agora", "device.credential")
}
