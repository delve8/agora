package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

type Config struct {
	ID             string
	Version        string
	ServerURL      string
	Credential     string
	CredentialPath string
	DatabasePath   string
	ClaudeBinary   string
	HomeDir        string
	Heartbeat      time.Duration
	OutboxLimit    int
}

type Daemon struct {
	config  Config
	store   *store.Store
	manager *runtime.Manager
	connMu  sync.Mutex
	writeMu sync.Mutex
	conn    *websocket.Conn
	outbox  *outbox
	eventMu sync.Mutex
	events  map[string]context.CancelFunc
}

func New(config Config) (*Daemon, error) {
	if config.ID == "" {
		config.ID = protocol.NewID("daemon")
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
	db, err := store.Open(config.DatabasePath)
	if err != nil {
		return nil, err
	}
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(config.ClaudeBinary), runtime.NewPTYManager(config.ClaudeBinary, config.HomeDir))
	return &Daemon{config: config, store: db, manager: manager, outbox: newOutbox(config.OutboxLimit), events: make(map[string]context.CancelFunc)}, nil
}

func (d *Daemon) Close() error {
	d.eventMu.Lock()
	for id, cancel := range d.events {
		cancel()
		delete(d.events, id)
	}
	d.eventMu.Unlock()
	if d.manager != nil {
		d.manager.Close()
	}
	if d.store != nil {
		return d.store.Close()
	}
	return nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if d.config.ServerURL == "" {
		return errors.New("daemon server URL is required")
	}
	if err := d.manager.ReconcileObservers(ctx); err != nil {
		return err
	}
	if err := d.manager.ResumeManagedSessions(ctx); err != nil {
		return err
	}
	values, err := d.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Source == session.SourceManaged {
			d.startEventBridge(value)
		}
	}
	for {
		if err := d.connect(ctx); err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if err := d.session(ctx); err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
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
	conn.SetReadLimit(1 << 20)
	d.connMu.Lock()
	d.conn = conn
	d.connMu.Unlock()
	return nil
}

func (d *Daemon) session(ctx context.Context) error {
	defer d.disconnect()
	if err := d.send(protocol.DaemonRegister, protocol.DaemonRegisterPayload{DaemonID: d.config.ID, Version: d.config.Version, Hostname: hostname(), Capabilities: map[string]bool{"can_start": true, "can_attach": true, "can_observe": true, "can_send_input": true, "can_stream": true, "can_resume": true, "can_approve": false, "can_read_history": true}}); err != nil {
		return err
	}
	if err := d.flushOutbox(); err != nil {
		return err
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(d.config.Heartbeat)
		defer ticker.Stop()
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
					heartbeatDone <- err
					return
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
		return d.sendResync()
	case protocol.ServerResyncRequest:
		return d.sendResync()
	case protocol.DaemonHeartbeatAck:
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
		value, err := d.manager.CreateManagedSessionWithID(context.Background(), payload.SessionID, payload.CoordinationID, payload.Workspace, payload.DisplayName, payload.Role)
		created := protocol.SessionCreatedPayload{SessionID: payload.SessionID}
		if err != nil {
			created.Error = err.Error()
			return d.sendResponse(protocol.SessionCreated, created, frame.RequestID)
		}
		created.PID = value.ProcessID
		created.ClaudeSessionID = value.ClaudeSessionID
		created.HistoryPath = value.HistoryPath
		created.Capabilities = map[string]bool{"can_start": true, "can_attach": true, "can_observe": true, "can_send_input": true, "can_stream": true, "can_resume": true, "can_approve": false, "can_read_history": true}
		if err := d.sendResponse(protocol.SessionCreated, created, frame.RequestID); err != nil {
			return err
		}
		d.startEventBridge(value)
		return d.send(protocol.SessionUpdate, protocol.SessionUpdatePayload{SessionID: payload.SessionID, ClaudeSessionID: value.ClaudeSessionID, State: value.State, Connection: value.Connection, PID: value.ProcessID, LastError: value.LastError})
	case protocol.SessionInput:
		var payload protocol.InputPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		value := mustSession(d.store, payload.SessionID)
		result := protocol.InputResultPayload{SessionID: payload.SessionID}
		if value.ID == "" {
			result.Error = fmt.Sprintf("session %s not found", payload.SessionID)
		} else if err := d.manager.Send(context.Background(), value, messageForInput(payload.Content)); err != nil {
			result.Error = err.Error()
		} else {
			result.Accepted = true
		}
		return d.sendResponse(protocol.SessionInputResult, result, frame.RequestID)
	case protocol.SessionHistoryRequest:
		var payload protocol.HistoryRequestPayload
		if err := protocol.DecodePayload(frame, &payload); err != nil {
			return err
		}
		response := protocol.HistoryResponsePayload{SessionID: payload.SessionID}
		values, err := d.store.ListEvents(context.Background(), payload.SessionID)
		if err != nil {
			response.Error = err.Error()
		} else {
			limit := payload.Limit
			if limit <= 0 || limit > len(values) {
				limit = len(values)
			}
			if limit < len(values) {
				values = values[len(values)-limit:]
			}
			body, marshalErr := json.Marshal(values)
			if marshalErr != nil {
				response.Error = marshalErr.Error()
			} else {
				response.Events = body
			}
		}
		return d.sendResponse(protocol.SessionHistoryResponse, response, frame.RequestID)
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

func (d *Daemon) sendResync() error {
	values, err := d.store.ListSessions(context.Background(), "")
	if err != nil {
		return err
	}
	payload := protocol.ResyncPayload{DaemonID: d.config.ID, Gap: d.outbox.Gap()}
	for _, value := range values {
		if value.Source != session.SourceManaged {
			continue
		}
		payload.Sessions = append(payload.Sessions, protocol.SessionSummary{
			SessionID:       value.ID,
			ClaudeSessionID: value.ClaudeSessionID,
			State:           value.State,
			Connection:      value.Connection,
			PID:             value.ProcessID,
		})
	}
	return d.send(protocol.DaemonResync, payload)
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
	d.connMu.Unlock()
}

func hostname() string {
	value, _ := os.Hostname()
	return value
}

func mustSession(db *store.Store, id string) session.Session {
	value, err := db.GetSession(context.Background(), id)
	if err != nil {
		return session.Session{}
	}
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
