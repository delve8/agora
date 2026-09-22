package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/runtime/piextension"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/sessionhost"
	"github.com/delve8/agora/internal/store"
	"github.com/delve8/agora/internal/terminal"
)

// rekeyAlias is one redirect from an id a session used to have to the id it has
// now. The canonical Session ID is derived from the provider's native id, so a
// provider context switch renames a session while a caller may still be holding
// the previous id. Without the redirect, an attach that races the switch is
// answered with "session not found" even though the Agent is running.
// AgentExit reports that a managed Agent process ended. The Session Host owns
// the process, so this is how its exit reaches the Daemon and the Server.
type AgentExit struct {
	AgoraID       string
	ClaudeSession string
	ExitCode      int
	Err           error
	Intentional   bool
}

type rekeyAlias struct {
	target string
	seq    uint64
}

// maxRekeyAliases bounds the redirect table: a Daemon runs for weeks and every
// creation and provider switch adds one entry.
const maxRekeyAliases = 512

type Manager struct {
	store            StateStore
	adapter          *adapter.ClaudeCodeAdapter
	claude           *ClaudeProvider
	pi               *PiProvider
	daemonID         string
	homeDir          string
	history          *adapter.HistoryCatalog
	piHistory        *adapter.PiHistoryCatalog
	piCatalogCache   *adapter.PiHistoryCatalog
	catalogs         []SessionCatalog
	catalogSnapshots map[string][]session.Session

	mu               sync.Mutex
	observers        map[string]context.CancelFunc
	observerTokens   map[string]uint64
	piObservers      map[string]context.CancelFunc
	piObserverTokens map[string]uint64
	subs             map[string]map[chan event.Event]struct{}
	sessionOnExit    func(session.Session, AgentExit)
	sessionRebind    func(string, session.Session)
	sessionUpdate    func(session.Session)
	eventHandler     func(event.Event)
	switchWatchers   map[string]*switchWatcher
	switchCache      map[string]switchCatalogCache
	hosts            *SessionHostRegistry
	hostWatchCancels map[string]context.CancelFunc
	hostWatchTokens  map[string]uint64
	rekeyMu          sync.Mutex
	rekeys           map[string]rekeyAlias
	rekeySeq         uint64
	closed           bool
}

func NewManager(db StateStore, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *ClaudeProvider) *Manager {
	homeDir, _ := os.UserHomeDir()
	if ptyManager != nil && ptyManager.homeDir != "" {
		homeDir = ptyManager.homeDir
	}
	manager := &Manager{store: db, adapter: agentAdapter, claude: ptyManager, homeDir: homeDir, history: adapter.NewHistoryCatalog(homeDir), piHistory: adapter.NewPiHistoryCatalog(homeDir, ""), catalogSnapshots: make(map[string][]session.Session), observers: make(map[string]context.CancelFunc), observerTokens: make(map[string]uint64), piObservers: make(map[string]context.CancelFunc), piObserverTokens: make(map[string]uint64), subs: make(map[string]map[chan event.Event]struct{}), switchWatchers: make(map[string]*switchWatcher), switchCache: make(map[string]switchCatalogCache), hostWatchCancels: make(map[string]context.CancelFunc), hostWatchTokens: make(map[string]uint64), rekeys: make(map[string]rekeyAlias)}
	manager.catalogs = manager.newSessionCatalogs()
	return manager
}

func NewPiProviderRuntime(db StateStore, daemonID string, config PiConfig) *Manager {
	manager := NewManager(db, nil, nil)
	manager.daemonID = daemonID
	manager.AttachPi(NewPiProvider(config))
	return manager
}

func (m *Manager) AttachPi(pi *PiProvider) {
	m.pi = pi
	if pi != nil {
		// Inject the Agora reporter extension so a provider context switch is
		// reported by the Agent itself. Failure only disables that fast path:
		// the evidence-based watcher still detects switches.
		if path, err := piextension.ReporterPath(m.homeDir); err == nil {
			pi.SetReporterExtension(path)
		} else {
			log.Printf("agora: Pi session reporter extension unavailable: %v", err)
		}
		m.mu.Lock()
		hasPiCatalog := false
		for _, catalog := range m.catalogs {
			if catalog.Provider() == "pi" {
				hasPiCatalog = true
				break
			}
		}
		if !hasPiCatalog {
			m.catalogs = append(m.catalogs, sessionCatalogFunc{provider: "pi", list: func(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
				return m.discoverPiHistory(ctx, coordinationID, owner)
			}})
		}
		m.mu.Unlock()
	}
}

// RegisterSessionCatalog adds a provider history catalog to the manager. The
// catalog is consulted during every discovery refresh and is intentionally
// kept in memory; provider history remains owned by the provider.
func (m *Manager) RegisterSessionCatalog(catalog SessionCatalog) error {
	if catalog == nil || strings.TrimSpace(catalog.Provider()) == "" {
		return errors.New("session catalog and provider are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.catalogs {
		if existing.Provider() == catalog.Provider() {
			return fmt.Errorf("session catalog for provider %q is already registered", catalog.Provider())
		}
	}
	m.catalogs = append(m.catalogs, catalog)
	return nil
}

func (m *Manager) EnableSessionHosts(executable string) {
	m.mu.Lock()
	m.hosts = NewSessionHostRegistryWithHome(executable, m.homeDir)
	m.mu.Unlock()
}

func NewDaemonManager(db StateStore, daemonID string, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *ClaudeProvider) *Manager {
	manager := NewManager(db, agentAdapter, ptyManager)
	manager.daemonID = daemonID
	// Host mode is explicitly enabled by the daemon once its executable path is known.
	return manager
}

// AdoptSessionHosts reconnects to hosts that survived a Daemon restart and
// recreates their in-memory session rows. It never launches a second Agent.
func (m *Manager) AdoptSessionHosts(ctx context.Context) error {
	if m == nil || m.hosts == nil {
		return nil
	}
	values, err := m.hosts.Adopt(ctx)
	if err != nil {
		return err
	}
	for _, metadata := range values {
		if metadata.SessionID == "" || metadata.Agent == "" {
			continue
		}
		value := session.Session{ID: metadata.SessionID, CoordinationID: metadata.CoordinationID, DaemonID: metadata.DaemonID, Agent: metadata.Agent, AgentSessionID: metadata.AgentSessionID, Workspace: metadata.Workspace, DisplayName: metadata.DisplayName, HistoryPath: metadata.HistoryPath, State: session.StateRunning, Source: session.SourceManaged, Connection: session.ConnectionObserved, ProcessID: metadata.AgentPID, Capabilities: managedHostCapabilities(), CreatedAt: metadata.CreatedAt, UpdatedAt: metadata.UpdatedAt}
		applyManagedHistoryCapability(&value)
		if value.Agent == "claude" {
			value.ClaudeSessionID = strings.TrimPrefix(value.AgentSessionID, "claude://")
		}
		if err := m.store.CreateSession(ctx, value); err != nil {
			// A Server/Daemon restart may already have restored the control
			// record. Adoption must attach to the Host, not duplicate it.
			if _, getErr := m.store.GetSession(ctx, value.ID); getErr == nil {
				m.monitorHost(value.ID)
				continue
			}
			return err
		}
		if value.HistoryPath != "" || value.Agent == "pi" {
			_ = m.StartObserver(value)
		}
		m.monitorHost(value.ID)
	}
	return nil
}

func managedHostCapabilities() session.Capabilities {
	return session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true}
}

// applyManagedHistoryCapability keeps a live managed session honest about
// history. Agora creates the provider session before the provider writes
// anything, and a context switch can point at a session whose transcript does
// not exist yet, so claiming CanReadHistory there shows the user an empty
// conversation that looks like lost history. The observer flips it back on as
// soon as it resolves the transcript.
func applyManagedHistoryCapability(value *session.Session) {
	path := strings.TrimSpace(value.HistoryPath)
	if path == "" {
		value.Capabilities.CanReadHistory = false
		return
	}
	_, err := os.Stat(path)
	value.Capabilities.CanReadHistory = err == nil
}

// hostSessionID resolves the canonical session id a Host client is currently
// registered under. A rebind moves the client to a new key, so the id captured
// when a subscription started must never be reused for later input lines.
func (m *Manager) hostSessionID(client *sessionhost.Client) string {
	if m == nil || m.hosts == nil || client == nil {
		return ""
	}
	for id, current := range m.hosts.Clients() {
		if current == client {
			return id
		}
	}
	return ""
}

func (m *Manager) stopHostMonitor(id string) {
	m.mu.Lock()
	if cancel := m.hostWatchCancels[id]; cancel != nil {
		cancel()
		delete(m.hostWatchCancels, id)
	}
	m.mu.Unlock()
}

func (m *Manager) monitorHost(id string) {
	if m == nil || m.hosts == nil {
		return
	}
	m.stopHostMonitor(id)
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.hostWatchCancels[id] = cancel
	m.hostWatchTokens[id]++
	token := m.hostWatchTokens[id]
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			if m.hostWatchTokens[id] == token {
				delete(m.hostWatchCancels, id)
			}
			m.mu.Unlock()
		}()
		// Keep the provider-neutral input trigger working when the PTY is owned
		// by a Session Host rather than by this Daemon process. The subscription
		// resolves the canonical id on every line because a runtime /resume
		// rebind rekeys the registry while this Host stays alive.
		if client, ok := m.hosts.Get(id); ok {
			go func(host *sessionhost.Client) {
				_ = host.Subscribe(ctx, func(line string) {
					if current := m.hostSessionID(host); current != "" {
						m.handleAgentInput(current, line)
					}
				})
			}(client)
		}
		for {
			if m.isClosed() {
				return
			}
			client, ok := m.hosts.Get(id)
			if !ok {
				return
			}
			if _, err := client.State(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				m.hosts.Delete(id)
				m.stopSwitchWatcher(id)
				if value, getErr := m.store.GetSession(context.Background(), id); getErr == nil {
					value.ProcessID = 0
					value.State = session.StateStopped
					value.Connection = session.ConnectionUnavailable
					_ = m.store.UpdateSessionObservation(context.Background(), value)
					m.mu.Lock()
					handler := m.sessionOnExit
					m.mu.Unlock()
					if handler != nil {
						handler(value, AgentExit{AgoraID: id})
					}
				}
				return
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}()
}
func (m *Manager) ListSessions(ctx context.Context, coordinationID string) ([]session.Session, error) {
	return m.store.ListSessions(ctx, coordinationID)
}

func (m *Manager) GetSession(ctx context.Context, id string) (session.Session, error) {
	if m == nil || m.store == nil {
		return session.Session{}, errSessionNotFound
	}
	return m.store.GetSession(ctx, m.ResolveSessionID(id))
}

// ResolveSessionID follows the redirects left behind by atomic session rekeys.
// It answers with id itself when the id was never rekeyed, so callers can use it
// to ask both "which session is this" and "did this id move".
func (m *Manager) ResolveSessionID(id string) string {
	if m == nil {
		return id
	}
	m.rekeyMu.Lock()
	defer m.rekeyMu.Unlock()
	return m.resolveRekeyLocked(id)
}

// resolveRekeyLocked follows a redirect chain. rememberRekey collapses chains so
// this normally takes one hop; the loop guard keeps a corrupted table from
// spinning forever.
func (m *Manager) resolveRekeyLocked(id string) string {
	for hops := 0; hops < 8; hops++ {
		alias, ok := m.rekeys[id]
		if !ok || alias.target == "" || alias.target == id {
			return id
		}
		id = alias.target
	}
	return id
}

// rememberRekey records that oldID now names the session at newID. Every
// redirect that already pointed at oldID is re-pointed, so a session that is
// renamed more than once (and one that returns to an id it used before) keeps a
// single-hop table without cycles.
func (m *Manager) rememberRekey(oldID, newID string) {
	if m == nil || oldID == "" || newID == "" || oldID == newID {
		return
	}
	m.rekeyMu.Lock()
	defer m.rekeyMu.Unlock()
	if m.rekeys == nil {
		m.rekeys = make(map[string]rekeyAlias)
	}
	repoint := make([]string, 0, 2)
	for key := range m.rekeys {
		if m.resolveRekeyLocked(key) == oldID {
			repoint = append(repoint, key)
		}
	}
	for _, key := range repoint {
		m.rekeys[key] = rekeyAlias{target: newID, seq: m.rekeys[key].seq}
	}
	// newID names a real session now, so it must not stay a redirect itself.
	delete(m.rekeys, newID)
	m.rekeySeq++
	m.rekeys[oldID] = rekeyAlias{target: newID, seq: m.rekeySeq}
	m.evictRekeysLocked()
}

// evictRekeysLocked drops the oldest redirect when the table is full. Oldest is
// the lowest sequence that is still present; a key re-remembered after eviction
// gets a fresh sequence.
func (m *Manager) evictRekeysLocked() {
	for len(m.rekeys) > maxRekeyAliases {
		oldestKey, oldestSeq := "", uint64(0)
		for key, alias := range m.rekeys {
			if oldestKey == "" || alias.seq < oldestSeq {
				oldestKey, oldestSeq = key, alias.seq
			}
		}
		if oldestKey == "" {
			return
		}
		delete(m.rekeys, oldestKey)
	}
}

// WaitAgentSessionID reports the native session id the Host that owns id was
// started with. The Daemon assigns it before the Agent starts (Claude through
// --session-id, Pi the same way whenever Agora controls the argv), so it is
// available without waiting for the provider to write anything.
func (m *Manager) WaitAgentSessionID(ctx context.Context, id string) (string, error) {
	if m == nil || m.hosts == nil {
		return "", fmt.Errorf("session hosts are unavailable")
	}
	client, ok := m.hosts.Get(id)
	if !ok {
		return "", fmt.Errorf("managed session %s has no session-host", id)
	}
	metadata, err := client.State(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(metadata.AgentSessionID, strings.TrimSpace(metadata.Agent)+"://"), nil
}

func (m *Manager) SetAgentIdentity(ctx context.Context, id, agent, agentSessionID string) (session.Session, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	value.Agent = agent
	value.AgentSessionID = agentSessionID
	if agent == "claude" {
		value.ClaudeSessionID = strings.TrimPrefix(agentSessionID, "claude://")
		value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	} else if agent == "pi" {
		nativeID := strings.TrimPrefix(agentSessionID, "pi://")
		value.HistoryPath = adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), nativeID)
	}
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	return value, nil
}

// piCatalog returns the shared Pi history catalog. The catalog caches parsed
// summaries per file, so history discovery, live-session enrichment and the
// switch watcher must reuse one instance instead of building a new one (and
// re-parsing every transcript) on each call.
func (m *Manager) piCatalog() *adapter.PiHistoryCatalog {
	root := m.piHistoryRoot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.piCatalogCache == nil || m.piCatalogCache.Root() != root {
		m.piCatalogCache = adapter.NewPiHistoryCatalog(m.homeDir, root)
	}
	return m.piCatalogCache
}

func (m *Manager) piHistoryRoot() string {
	if m.piHistory == nil {
		return ""
	}
	if m.pi != nil && m.pi.SessionDir() != "" {
		return m.pi.SessionDir()
	}
	return m.piHistory.Root()
}

func (m *Manager) newSessionCatalogs() []SessionCatalog {
	catalogs := []SessionCatalog{
		sessionCatalogFunc{provider: "claude", list: func(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
			return m.discoverClaudeHistory(ctx, coordinationID, owner)
		}},
	}
	if m.pi != nil {
		catalogs = append(catalogs, sessionCatalogFunc{provider: "pi", list: func(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
			return m.discoverPiHistory(ctx, coordinationID, owner)
		}})
	}
	return catalogs
}

func (m *Manager) RekeySession(ctx context.Context, oldID, newID string) (session.Session, error) {
	value, err := m.store.GetSession(ctx, oldID)
	if err != nil {
		return session.Session{}, err
	}
	if m.hosts != nil {
		if client, ok := m.hosts.Get(oldID); ok {
			if _, err := client.Rebind(ctx, newID, value.AgentSessionID, value.HistoryPath, value.DisplayName); err != nil {
				return session.Session{}, err
			}
			m.hosts.Delete(oldID)
			m.hosts.Put(newID, client)
		}
	}
	m.StopObserver(oldID)
	value.ID = newID
	if err := m.store.RekeySession(ctx, oldID, value); err != nil {
		return session.Session{}, err
	}
	m.rememberRekey(oldID, newID)
	m.mu.Lock()
	m.mu.Unlock()
	if value.HistoryPath != "" || value.Agent == "pi" {
		_ = m.StartObserver(value)
	}
	return value, nil
}

func (m *Manager) Subscribe(coordinationID string) (<-chan event.Event, func()) {
	ch := make(chan event.Event, 64)
	m.mu.Lock()
	if m.closed {
		close(ch)
		m.mu.Unlock()
		return ch, func() {}
	}
	if m.subs[coordinationID] == nil {
		m.subs[coordinationID] = make(map[chan event.Event]struct{})
	}
	m.subs[coordinationID][ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return
		}
		if subscribers := m.subs[coordinationID]; subscribers != nil {
			delete(subscribers, ch)
			if len(subscribers) == 0 {
				delete(m.subs, coordinationID)
			}
		}
		close(ch)
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, cancel := range m.observers {
		cancel()
	}
	for _, cancel := range m.piObservers {
		cancel()
	}
	for _, cancel := range m.hostWatchCancels {
		cancel()
	}
	m.observers = make(map[string]context.CancelFunc)
	m.hostWatchCancels = make(map[string]context.CancelFunc)
	m.hostWatchTokens = make(map[string]uint64)
	m.piObservers = make(map[string]context.CancelFunc)
	m.switchWatchers = make(map[string]*switchWatcher)
	m.switchCache = make(map[string]switchCatalogCache)
	for coordinationID, subscribers := range m.subs {
		for ch := range subscribers {
			close(ch)
		}
		delete(m.subs, coordinationID)
	}
	m.mu.Unlock()
	// Session Hosts own their Agent processes. Closing the Daemon must only
	// drop host clients; it must not send stop/kill to hosted sessions.
	if m.hosts != nil {
		m.hosts.Clear()
	}
}

// Send writes one turn into a managed session's PTY and returns after the input
// is accepted. The Session Host owns the Agent, so the turn's completion is
// observed from the provider history rather than tracked here.
func (m *Manager) Send(ctx context.Context, value session.Session, msg message.Message) error {
	if m == nil || m.hosts == nil {
		return fmt.Errorf("session hosts are unavailable")
	}
	client, ok := m.hosts.Get(value.ID)
	if !ok {
		return fmt.Errorf("session %s is not running", value.ID)
	}
	if !value.Capabilities.CanSendInput {
		return fmt.Errorf("session does not accept input")
	}
	if err := m.store.UpdateMessage(ctx, msg.ID, message.StatusSent, ""); err != nil {
		return err
	}
	if err := client.Input(ctx, msg.Content); err != nil {
		_ = m.store.UpdateMessage(context.Background(), msg.ID, message.StatusFailed, err.Error())
		m.publishError(value, err)
		return err
	}
	return nil
}

func (m *Manager) CreateManagedSessionWithID(ctx context.Context, id, coordinationID, workspace, displayName, role string) (session.Session, error) {
	return m.CreateManagedSessionWithAgent(ctx, id, coordinationID, workspace, displayName, role, "claude")
}

func (m *Manager) CreateManagedSessionWithAgent(ctx context.Context, id, coordinationID, workspace, displayName, role, agent string) (session.Session, error) {
	return m.CreateManagedSessionWithAgentArgs(ctx, id, coordinationID, workspace, displayName, role, agent, nil)
}

// CreateManagedSessionWithAgentArgs is the provider-neutral creation entry
// point. agentArgs are owned by the provider and are forwarded without
// parsing or rewriting.
// CreateManagedSessionWithAgentArgs is the provider-neutral creation entry
// point. agentArgs are owned by the provider and are forwarded without parsing
// or rewriting. Every Agent runs under its own Session Host, so a Manager
// without hosts cannot create anything.
func (m *Manager) CreateManagedSessionWithAgentArgs(ctx context.Context, id, coordinationID, workspace, displayName, role, agent string, agentArgs []string) (session.Session, error) {
	if m == nil || m.hosts == nil {
		return session.Session{}, fmt.Errorf("session hosts are unavailable")
	}
	switch agent {
	case "pi":
		if m.pi == nil {
			return session.Session{}, fmt.Errorf("Pi session manager is unavailable")
		}
		if err := m.pi.Err(); err != nil {
			return session.Session{}, err
		}
		return m.createHostedPiSession(ctx, id, coordinationID, workspace, displayName, role, agentArgs)
	case "claude", "claude-code":
		if len(agentArgs) > 0 {
			// Agora builds Claude's argv (--session-id / --resume), so create
			// arguments have nowhere to go; dropping them silently would start a
			// session that ignores what the caller asked for.
			return session.Session{}, fmt.Errorf("Claude sessions do not accept create arguments")
		}
		if m.claude == nil {
			return session.Session{}, fmt.Errorf("session manager is unavailable")
		}
		if err := m.claude.Err(); err != nil {
			return session.Session{}, err
		}
		return m.createHostedClaudeSession(ctx, id, coordinationID, workspace, displayName, role)
	default:
		return session.Session{}, fmt.Errorf("unsupported agent %q", agent)
	}
}

func (m *Manager) createHostedClaudeSession(ctx context.Context, id, coordinationID, workspace, displayName, role string) (session.Session, error) {
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{ID: id, CoordinationID: coordinationID, DaemonID: m.daemonID, Agent: "claude", Workspace: workspace, DisplayName: displayName, DisplayNameSource: session.InitialDisplayNameSource(displayName), Role: role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: managedRunningCapabilities(), CreatedAt: now, UpdatedAt: now}
	if err := m.store.CreateSession(ctx, value); err != nil {
		return session.Session{}, err
	}
	nativeID, err := newNativeSessionID()
	if err != nil {
		return session.Session{}, err
	}
	value.AgentSessionID = "claude://" + nativeID
	client, err := m.hosts.Spawn(ctx, value, m.claude.FreshCommand(nativeID), agentEnv(ctx, cleanClaudeEnv()))
	if err != nil {
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		value.LastError = err.Error()
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, id)
	}
	metadata, err := client.State(ctx)
	if err != nil {
		return session.Session{}, err
	}
	value.ProcessID = metadata.AgentPID
	value.HistoryPath = metadata.HistoryPath
	value.ClaudeSessionID = nativeID
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Capabilities = managedHostCapabilities()
	applyManagedHistoryCapability(&value)
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	if err := m.StartObserver(value); err != nil {
		return session.Session{}, err
	}
	m.monitorHost(id)
	return m.store.GetSession(ctx, id)
}

func (m *Manager) createHostedPiSession(ctx context.Context, id, coordinationID, workspace, displayName, role string, agentArgs []string) (session.Session, error) {
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{ID: id, CoordinationID: coordinationID, DaemonID: m.daemonID, Agent: "pi", Workspace: workspace, DisplayName: displayName, DisplayNameSource: session.InitialDisplayNameSource(displayName), Role: role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: piCapabilities(), CreatedAt: now, UpdatedAt: now}
	if err := m.store.CreateSession(ctx, value); err != nil {
		return session.Session{}, err
	}
	nativeID, err := newPiNativeID()
	if err != nil {
		return session.Session{}, err
	}
	value.AgentSessionID = "pi://" + nativeID
	client, err := m.hosts.Spawn(ctx, value, m.pi.Command(nativeID, "", agentArgs), agentEnv(ctx, cleanPiEnv()))
	if err != nil {
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		value.LastError = err.Error()
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, id)
	}
	metadata, err := client.State(ctx)
	if err != nil {
		return session.Session{}, err
	}
	value.ProcessID = metadata.AgentPID
	value.HistoryPath = metadata.HistoryPath
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Capabilities = piCapabilities()
	applyManagedHistoryCapability(&value)
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	if err := m.StartObserver(value); err != nil {
		return session.Session{}, err
	}
	m.monitorHost(id)
	return m.store.GetSession(ctx, id)
}

func shouldEnrichLiveName(value session.Session) bool {
	name := strings.TrimSpace(value.DisplayName)
	if name == "" || name == "." || session.IsGeneratedDisplayName(name) {
		return true
	}
	return value.Workspace != "" && filepath.Base(filepath.Clean(value.Workspace)) == name
}

func piCapabilities() session.Capabilities {
	// Pi runs its native TUI under an Agora-owned PTY. The same VT emulator
	// used by Claude exposes a read-only screen snapshot to the Web UI.
	return session.Capabilities{CanStart: true, CanDiscover: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
}

func newNativeSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Pi session id: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(value[0:4]), hex.EncodeToString(value[4:6]), hex.EncodeToString(value[6:8]), hex.EncodeToString(value[8:10]), hex.EncodeToString(value[10:16])), nil
}

func newPiNativeID() (string, error) {
	return newNativeSessionID()
}

// longer relaunched automatically: persisted state is reconciled against the
// actual PTY table and users resume inactive conversations explicitly.
func (m *Manager) ResumeManagedSessions(ctx context.Context) error {
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Source != session.SourceManaged || m.IsRunning(value.ID) {
			continue
		}
		effective := m.EffectiveSession(value)
		if err := m.store.UpdateSessionObservation(ctx, effective); err != nil {
			return err
		}
	}
	return nil
}

func isSessionNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, errSessionNotFound)
}

func isCursorNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, errCursorNotFound)
}

func managedRunningCapabilities() session.Capabilities {
	return session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
}

// ResumeSession restarts a stored session under a fresh Session Host.
func (m *Manager) ResumeSession(ctx context.Context, value session.Session) (session.Session, error) {
	if m == nil || m.hosts == nil {
		return session.Session{}, fmt.Errorf("session hosts are unavailable")
	}
	if value.Agent != "pi" && value.Agent != "claude" && value.Agent != "claude-code" {
		return session.Session{}, fmt.Errorf("unsupported agent %q", value.Agent)
	}
	return m.resumeHostedSession(ctx, value)
}

func (m *Manager) resumeHostedSession(ctx context.Context, value session.Session) (session.Session, error) {
	agent := strings.ToLower(strings.TrimSpace(value.Agent))
	if agent == "claude-code" {
		agent = "claude"
	}
	if agent != "pi" && agent != "claude" {
		return session.Session{}, fmt.Errorf("unsupported hosted agent %q", value.Agent)
	}
	if value.NativeSessionURI() == "" {
		return session.Session{}, fmt.Errorf("%s session id is required", agent)
	}
	workspace, err := filepath.Abs(strings.TrimSpace(value.Workspace))
	if err != nil {
		return session.Session{}, err
	}
	if info, statErr := os.Stat(workspace); statErr != nil || !info.IsDir() {
		return session.Session{}, fmt.Errorf("workspace must be an existing directory")
	}
	value.Agent = agent
	value.Workspace = workspace
	if value.AgentSessionID == "" {
		value.AgentSessionID = value.NativeSessionURI()
	}
	if agent == "claude" {
		value.ClaudeSessionID = strings.TrimPrefix(value.AgentSessionID, "claude://")
	}
	// A resumed conversation may still only exist as a discovered history row,
	// so make sure the session is stored before the Host reports into it.
	if _, err := m.store.GetSession(ctx, value.ID); isSessionNotFound(err) {
		value.Source = session.SourceManaged
		value.State = session.StateStarting
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		value.Capabilities = session.Capabilities{CanReadHistory: true, CanResume: true}
		if value.CreatedAt.IsZero() {
			value.CreatedAt = time.Now().UTC()
		}
		value.UpdatedAt = time.Now().UTC()
		if err := m.store.CreateSession(ctx, value); err != nil {
			return session.Session{}, err
		}
	} else if err != nil {
		return session.Session{}, err
	}
	var command []string
	var env []string
	if agent == "pi" {
		nativeID := strings.TrimPrefix(value.AgentSessionID, "pi://")
		if m.pi == nil {
			return session.Session{}, fmt.Errorf("Pi session manager is unavailable")
		}
		command = m.pi.Command(nativeID, value.HistoryPath, nil)
		env = agentEnv(ctx, cleanPiEnv())
	} else {
		if m.claude == nil {
			return session.Session{}, fmt.Errorf("session manager is unavailable")
		}
		command = m.claude.Command(value.ClaudeSessionID)
		env = agentEnv(ctx, cleanClaudeEnv())
	}
	client, err := m.hosts.Spawn(ctx, value, command, env)
	if err != nil {
		return session.Session{}, err
	}
	metadata, err := client.State(ctx)
	if err != nil {
		return session.Session{}, err
	}
	value.ProcessID = metadata.AgentPID
	value.HistoryPath = firstNonEmpty(value.HistoryPath, metadata.HistoryPath)
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Source = session.SourceManaged
	value.Capabilities = managedHostCapabilities()
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	if err := m.StartObserver(value); err != nil {
		return session.Session{}, err
	}
	m.monitorHost(value.ID)
	return m.store.GetSession(ctx, value.ID)
}

func (m *Manager) StopSession(value session.Session) error {
	if m == nil || m.hosts == nil {
		return fmt.Errorf("session hosts are unavailable")
	}
	client, ok := m.hosts.Get(value.ID)
	if !ok {
		return fmt.Errorf("session is not running")
	}
	return client.Stop(context.Background())
}

// DeleteSession permanently removes a stopped session: its provider transcript
// files, its observers and its stored row. Running sessions are refused so the
// caller can stop them first. The value may be a live row or a history row that
// was never persisted; only HistoryPath is required to delete the files.
func (m *Manager) DeleteSession(ctx context.Context, value session.Session) error {
	if m == nil {
		return fmt.Errorf("session manager is unavailable")
	}
	id := m.ResolveSessionID(value.ID)
	if m.IsRunning(id) {
		return fmt.Errorf("session is running; stop it before deleting")
	}
	m.StopObserver(id)
	if m.hosts != nil {
		m.hosts.Delete(id)
	}
	if err := deleteProviderHistory(value); err != nil {
		return err
	}
	if m.store != nil {
		if err := m.store.DeleteSession(ctx, id); err != nil && !isSessionNotFound(err) {
			return err
		}
	}
	m.forgetCatalogSession(id)
	return nil
}

// deleteProviderHistory removes the provider-owned transcript. Claude also
// keeps per-session side data (subagents, tool results) in a directory named
// after the transcript; leaving it behind would not actually free the space.
// Workspace files are never touched.
func deleteProviderHistory(value session.Session) error {
	path := strings.TrimSpace(value.HistoryPath)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	agent := strings.ToLower(strings.TrimSpace(value.Agent))
	if agent == "claude" || agent == "claude-code" {
		dir := strings.TrimSuffix(path, filepath.Ext(path))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// forgetCatalogSession drops a deleted session from the fallback snapshots so a
// transient catalog error cannot resurrect it before the next successful scan.
func (m *Manager) forgetCatalogSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for provider, values := range m.catalogSnapshots {
		filtered := values[:0]
		for _, value := range values {
			if value.ID != id {
				filtered = append(filtered, value)
			}
		}
		m.catalogSnapshots[provider] = filtered
	}
}

func (m *Manager) StartObserver(value session.Session) error {
	if value.Agent == "pi" {
		return m.StartPiObserver(value)
	}
	m.startSwitchWatcher(value)
	m.mu.Lock()
	if _, exists := m.observers[value.ID]; exists {
		m.mu.Unlock()
		return nil
	}
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("session manager is closed")
	}
	// Observers are manager-owned and live until Close; they must not derive
	// from a request context, which is cancelled when the HTTP call returns.
	ctx, cancel := context.WithCancel(context.Background())
	m.observers[value.ID] = cancel
	m.observerTokens[value.ID]++
	token := m.observerTokens[value.ID]
	m.mu.Unlock()
	// Establish the initial end-of-file cursor before returning to the caller.
	// Otherwise a very fast append immediately after ResumeSession can race
	// with the observer goroutine's first stat and get skipped as "old".
	m.initializeObserverCursor(value.ID, value.HistoryPath)
	go m.observe(ctx, value, token)
	return nil
}

func (m *Manager) StopObserver(id string) {
	if value, err := m.store.GetSession(context.Background(), id); err == nil && value.Agent == "pi" {
		m.StopPiObserver(id)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopObserverLocked(id)
}

func (m *Manager) stopObserverLocked(id string) {
	if cancel := m.observers[id]; cancel != nil {
		cancel()
		delete(m.observers, id)
	}
	m.stopSwitchWatcherLocked(id)
}

func (m *Manager) stopObserverIfCurrent(id string, token uint64) {
	m.mu.Lock()
	if m.observerTokens[id] == token {
		m.stopObserverLocked(id)
	}
	m.mu.Unlock()
}

// ReconcileObservers starts observers for all persisted sessions that have a
// history path (both fresh managed launches and sessions restored after an
// Agora restart). It does not launch Claude — that is ClaudeProvider's job.
func (m *Manager) ReconcileObservers(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.HistoryPath == "" || !m.IsRunning(value.ID) {
			continue
		}
		if err := m.StartObserver(value); err != nil {
			value.State = session.StateStale
			value.Connection = session.ConnectionStale
			value.LastError = err.Error()
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
	}
	return nil
}

func (m *Manager) observe(ctx context.Context, value session.Session, token uint64) {
	defer m.stopObserverIfCurrent(value.ID, token)
	cursor, err := m.store.GetObservationCursor(ctx, value.ID)
	if isCursorNotFound(err) {
		cursor = store.ObservationCursor{SessionID: value.ID, Path: value.HistoryPath}
	} else if err != nil {
		m.markObservationError(value, err)
		return
	}
	if cursor.Path == "" {
		cursor.Path = value.HistoryPath
	}
	if cursor.Path != "" {
		if info, statErr := os.Stat(cursor.Path); statErr == nil && cursor.ByteOffset == 0 && cursor.Line == 0 {
			cursor.ByteOffset = info.Size()
			_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset})
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		// Refresh the session from the DB each loop: claude_session_id and
		// history_path may be back-filled after launch (the metadata/JSONL only
		// appear once claude accepts the workspace trust prompt), so the
		// observer must not rely on the launch-time snapshot.
		if fresh, err := m.store.GetSession(ctx, value.ID); err == nil {
			value = fresh
		}
		// The history JSONL may appear only after claude accepts the workspace
		// trust prompt. Re-resolve it from the session's Claude id when empty.
		if cursor.Path == "" && value.ClaudeSessionID != "" {
			cursor.Path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
			if cursor.Path != "" {
				value.HistoryPath = cursor.Path
				applyManagedHistoryCapability(&value)
				_ = m.store.UpdateSessionObservation(ctx, value)
			}
		}
		records, readErr := adapter.ReadHistory(ctx, adapter.HistoryCursor{Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID}, value.ID)
		if readErr != nil {
			m.markObservationError(value, readErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, record := range records {
			name := ""
			nameSource := ""
			switch record.Event.Kind {
			case event.KindAITitle:
				if session.CanApplyAITitle(value) {
					name = session.DescribeMessage(record.Event.Content)
					nameSource = session.DisplayNameSourceAITitle
				}
			case event.KindUser:
				if session.CanApplyFirstUserName(value) {
					name = session.DescribeMessage(record.Event.Content)
					nameSource = session.DisplayNameSourceFirstUser
				}
			}
			if name != "" {
				value.DisplayName = name
				value.DisplayNameSource = nameSource
				_ = m.store.UpdateSessionDisplayName(ctx, value.ID, name, nameSource)
				m.notifySessionUpdate(value)
			}
			cursor.ByteOffset = record.Cursor.ByteOffset
			cursor.Line = record.Cursor.Line
			cursor.LastID = record.Event.ExternalID
			_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID})
			m.publish(value.CoordinationID, record.Event)
		}
		if len(records) > 0 && m.IsRunning(value.ID) {
			now := time.Now().UTC()
			value.LastObservedAt = &now
			value.Connection = session.ConnectionObserved
			value.State = session.StateWaiting
			value.LastError = ""
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) initializeObserverCursor(sessionID, historyPath string) {
	if m == nil || m.store == nil || strings.TrimSpace(historyPath) == "" {
		return
	}
	_, err := m.store.GetObservationCursor(context.Background(), sessionID)
	if err == nil {
		return
	}
	if !isCursorNotFound(err) {
		return
	}
	info, err := os.Stat(historyPath)
	if err != nil {
		return
	}
	_ = m.store.SaveObservationCursor(context.Background(), store.ObservationCursor{
		SessionID: sessionID, Path: historyPath, ByteOffset: info.Size(),
	})
}

func (m *Manager) markObservationError(value session.Session, err error) {
	value.Connection = session.ConnectionStale
	value.State = session.StateStale
	value.LastError = err.Error()
	_ = m.store.UpdateSessionObservation(context.Background(), value)
}

func (m *Manager) SetSessionExitHandler(handler func(session.Session, AgentExit)) {
	m.mu.Lock()
	m.sessionOnExit = handler
	m.mu.Unlock()
}

// SetSessionRebindHandler receives a provider context rebind after the
// runtime has matched a post-switch user message to a history append.
func (m *Manager) SetSessionRebindHandler(handler func(string, session.Session)) {
	m.mu.Lock()
	m.sessionRebind = handler
	m.mu.Unlock()
}

// SetSessionUpdateHandler receives live metadata changes discovered by an
// observer, such as a provider history title or a late history path. The
// daemon uses it to immediately refresh the Server instead of waiting for the
// next periodic resync.
func (m *Manager) SetSessionUpdateHandler(handler func(session.Session)) {
	m.mu.Lock()
	m.sessionUpdate = handler
	m.mu.Unlock()
}

func (m *Manager) notifySessionUpdate(value session.Session) {
	m.mu.Lock()
	handler := m.sessionUpdate
	m.mu.Unlock()
	if handler != nil {
		handler(value)
	}
}

func (m *Manager) SetEventHandler(handler func(event.Event)) {
	m.mu.Lock()
	m.eventHandler = handler
	m.mu.Unlock()
}

func (m *Manager) IsRunning(id string) bool {
	if m == nil || m.hosts == nil {
		return false
	}
	client, ok := m.hosts.Get(id)
	if !ok {
		return false
	}
	_, err := client.State(context.Background())
	return err == nil
}

// LiveSessions returns the sessions the local manager is currently running
// (serve mode). The manager owns these PTYs, so the live set is read from
// memory rather than persisted rows.
// LiveSessions returns the sessions whose Session Host is currently running.
// The Host owns the Agent process, so the live set is read from the Host
// registry and enriched from the stored session row.
func (m *Manager) LiveSessions(ctx context.Context, coordinationID string) ([]session.Session, error) {
	if m == nil || m.hosts == nil {
		return nil, nil
	}
	values := make([]session.Session, 0)
	for id, client := range m.hosts.Clients() {
		metadata, err := client.State(ctx)
		if err != nil {
			continue
		}
		value, err := m.store.GetSession(ctx, id)
		if err != nil || (coordinationID != "" && value.CoordinationID != coordinationID) {
			continue
		}
		value.Agent = metadata.Agent
		value.AgentSessionID = metadata.AgentSessionID
		value.Workspace = firstNonEmpty(value.Workspace, metadata.Workspace)
		value.DisplayName = firstNonEmpty(value.DisplayName, metadata.DisplayName)
		value.HistoryPath = firstNonEmpty(value.HistoryPath, metadata.HistoryPath)
		value.ProcessID = metadata.AgentPID
		value.State = session.StateRunning
		value.Connection = session.ConnectionObserved
		value.Capabilities = managedHostCapabilities()
		applyManagedHistoryCapability(&value)
		values = append(values, value)
	}
	return values, nil
}

func (m *Manager) EffectiveSession(value session.Session) session.Session {
	if value.Source == session.SourceHistory {
		value.Capabilities = session.Capabilities{CanReadHistory: true, CanResume: value.NativeSessionURI() != "" && value.Workspace != ""}
		value.State = session.StateStopped
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		return value
	}
	if value.Source != session.SourceManaged {
		return value
	}
	if m.IsRunning(value.ID) {
		if value.State != session.StateWaiting && value.State != session.StateStarting {
			value.State = session.StateRunning
		}
		value.Connection = session.ConnectionObserved
		if value.Agent == "pi" {
			value.Capabilities = piCapabilities()
		} else {
			value.Capabilities = managedRunningCapabilities()
		}
		return value
	}
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.ProcessID = 0
	value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || value.NativeSessionURI() != "", CanResume: value.NativeSessionURI() != "" && value.Workspace != ""}
	return value
}

func (m *Manager) DiscoverHistorySessions(ctx context.Context, coordinationID, daemonID string) ([]session.Session, error) {
	return m.listCatalogSessions(ctx, coordinationID, daemonID)
}

func (m *Manager) listCatalogSessions(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
	m.mu.Lock()
	catalogs := append([]SessionCatalog(nil), m.catalogs...)
	m.mu.Unlock()
	values := make([]session.Session, 0)
	for _, catalog := range catalogs {
		found, err := catalog.List(ctx, coordinationID, owner)
		if err != nil {
			// A provider's history source may be temporarily unavailable (for
			// example, OpenCode's SQLite file can be locked). Do not make that
			// hide every other provider or erase the last known snapshot.
			m.mu.Lock()
			cached := append([]session.Session(nil), m.catalogSnapshots[catalog.Provider()]...)
			m.mu.Unlock()
			values = append(values, cached...)
			continue
		}
		m.mu.Lock()
		m.catalogSnapshots[catalog.Provider()] = append([]session.Session(nil), found...)
		m.mu.Unlock()
		values = append(values, found...)
	}
	return values, nil
}

func (m *Manager) discoverClaudeHistory(ctx context.Context, coordinationID, daemonID string) ([]session.Session, error) {
	summaries, err := m.history.List(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]session.Session, 0, len(summaries))
	for _, summary := range summaries {
		if summary.SessionID == "" {
			continue
		}
		uri := "claude://" + summary.SessionID
		id, err := session.NewSessionID(daemonID, "claude", uri)
		if err != nil {
			continue
		}
		name := session.DescribeMessage(summary.LatestAITitle)
		nameSource := session.DisplayNameSourceAITitle
		if name == "" {
			name = session.DescribeMessage(summary.FirstUser)
			nameSource = session.DisplayNameSourceFirstUser
		}
		if name == "" {
			name = summary.SessionID
			nameSource = session.DisplayNameSourceInitial
		}
		workspace := strings.TrimSpace(summary.Workspace)
		if workspace == "" {
			workspace = summary.ProjectDirectory
		}
		updatedAt := summary.LastEventAt
		if updatedAt.IsZero() {
			updatedAt = summary.ModifiedAt
		}
		createdAt := summary.FirstEventAt
		if createdAt.IsZero() {
			createdAt = updatedAt
		}
		values = append(values, session.Session{ID: id, CoordinationID: coordinationID, DaemonID: daemonID, Agent: "claude", AgentSessionID: uri, ClaudeSessionID: summary.SessionID, Workspace: workspace, DisplayName: name, DisplayNameSource: nameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: workspace != ""}, CreatedAt: createdAt, UpdatedAt: updatedAt})
	}

	return values, nil
}

func (m *Manager) discoverPiHistory(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
	piSummaries, err := m.piCatalog().List(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]session.Session, 0, len(piSummaries))
	for _, summary := range piSummaries {
		value, ok := piHistorySession(summary, coordinationID, owner)
		if ok {
			values = append(values, value)
		}
	}
	return values, nil
}

func (m *Manager) HistorySessions(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
	return m.listCatalogSessions(ctx, coordinationID, owner)
}

func (m *Manager) HistorySession(ctx context.Context, id, coordinationID, owner string) (session.Session, bool, error) {
	values, err := m.HistorySessions(ctx, coordinationID, owner)
	if err != nil {
		return session.Session{}, false, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, true, nil
		}
	}
	return session.Session{}, false, nil
}

func (m *Manager) ResolveHistory(ctx context.Context, id, coordinationID, owner string, limit int) ([]event.Event, error) {
	if value, err := m.store.GetSession(ctx, id); err == nil {
		return m.HistoryForSession(ctx, value, limit)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	value, ok, err := m.HistorySession(ctx, id, coordinationID, owner)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, sql.ErrNoRows
	}
	return m.HistoryForSession(ctx, value, limit)
}

func (m *Manager) HistoryForSession(ctx context.Context, value session.Session, limit int) ([]event.Event, error) {
	if value.Agent == "pi" {
		return m.PiHistoryForSession(ctx, value, limit)
	}
	if value.Source == session.SourceHistory {
		if value.HistoryPath == "" {
			return []event.Event{}, nil
		}
		values, err := adapter.ReadAllHistory(ctx, value.HistoryPath, value.ID, limit)
		if os.IsNotExist(err) {
			return []event.Event{}, nil
		}
		return values, err
	}
	return m.History(ctx, value.ID, limit)
}

func historySession(summary adapter.HistorySummary, coordinationID, owner string) session.Session {
	name := session.DescribeMessage(summary.LatestAITitle)
	source := session.DisplayNameSourceAITitle
	if name == "" {
		name = session.DescribeMessage(summary.FirstUser)
		source = session.DisplayNameSourceFirstUser
	}
	if name == "" {
		name = summary.SessionID
		source = session.DisplayNameSourceInitial
	}
	workspace := strings.TrimSpace(summary.Workspace)
	if workspace == "" {
		workspace = summary.ProjectDirectory
	}
	updated := summary.LastEventAt
	if updated.IsZero() {
		updated = summary.ModifiedAt
	}
	created := summary.FirstEventAt
	if created.IsZero() {
		created = updated
	}
	return session.Session{ID: historySessionID(owner, summary.SessionID), CoordinationID: coordinationID, Agent: "claude-code", ExternalID: summary.SessionID, AgentSessionID: "claude://" + summary.SessionID, ClaudeSessionID: summary.SessionID, Workspace: workspace, DisplayName: name, DisplayNameSource: source, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: summary.SessionID != "" && workspace != ""}, CreatedAt: created, UpdatedAt: updated}
}
func piHistorySession(summary adapter.PiHistorySummary, coordinationID, owner string) (session.Session, bool) {
	if summary.SessionID == "" {
		return session.Session{}, false
	}
	uri := "pi://" + summary.SessionID
	id, err := session.NewSessionID(owner, "pi", uri)
	if err != nil {
		return session.Session{}, false
	}
	name := session.DescribeMessage(summary.SessionName)
	nameSource := session.DisplayNameSourceCustom
	if name == "" {
		name = session.DescribeMessage(summary.FirstUser)
		nameSource = session.DisplayNameSourceFirstUser
	}
	if name == "" {
		name = piFallbackDisplayName(summary.Workspace)
		nameSource = session.DisplayNameSourceInitial
	}
	createdAt := summary.FirstEventAt
	if createdAt.IsZero() {
		createdAt = summary.ModifiedAt
	}
	updatedAt := summary.LastEventAt
	if updatedAt.IsZero() {
		updatedAt = summary.ModifiedAt
	}
	return session.Session{ID: id, CoordinationID: coordinationID, DaemonID: owner, Agent: "pi", AgentSessionID: uri, Workspace: summary.Workspace, DisplayName: name, DisplayNameSource: nameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: piCapabilitiesForHistory(summary.Workspace), CreatedAt: createdAt, UpdatedAt: updatedAt}, true
}

func piFallbackDisplayName(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace != "" {
		if base := filepath.Base(filepath.Clean(workspace)); base != "" && base != "." && base != string(filepath.Separator) {
			return "Pi · " + base
		}
	}
	return "Pi session"
}

func piCapabilitiesForHistory(workspace string) session.Capabilities {
	return session.Capabilities{CanReadHistory: true, CanResume: workspace != ""}
}

func historySessionID(owner, nativeID string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + nativeID))
	return fmt.Sprintf("history-%x", sum[:12])
}
func (m *Manager) CanManageSessions() bool { return m != nil && (m.claude != nil || m.pi != nil) }
func (m *Manager) History(ctx context.Context, id string, limit int) ([]event.Event, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if value.Agent == "pi" {
		return m.PiHistoryForSession(ctx, value, limit)
	}
	path := value.HistoryPath
	if path == "" && value.ClaudeSessionID != "" {
		path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
		if path != "" {
			value.HistoryPath = path
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
	}
	if path == "" {
		return []event.Event{}, nil
	}
	values, err := adapter.ReadAllHistory(ctx, path, id, limit)
	if os.IsNotExist(err) {
		return []event.Event{}, nil
	}
	return values, err
}
func (m *Manager) FirstUserEvent(ctx context.Context, id string) (event.Event, bool, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return event.Event{}, false, err
	}
	if value.Agent == "pi" {
		values, err := m.PiHistoryForSession(ctx, value, 0)
		if err != nil {
			return event.Event{}, false, err
		}
		for _, item := range values {
			if item.Kind == event.KindUser && strings.TrimSpace(item.Content) != "" {
				return item, true, nil
			}
		}
		return event.Event{}, false, nil
	}
	path := value.HistoryPath
	if path == "" && value.ClaudeSessionID != "" {
		path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	}
	if path == "" {
		return event.Event{}, false, nil
	}
	return adapter.FirstUserHistoryEvent(ctx, path, id)
}
func (m *Manager) DerivedDisplayName(ctx context.Context, id string) (string, string, bool, error) {
	values, err := m.History(ctx, id, 0)
	if err != nil {
		return "", "", false, err
	}
	name, source := session.DerivedDisplayName(values)
	return name, source, name != "", nil
}
func (m *Manager) Snapshot(id string) (terminal.Snapshot, error) {
	if m == nil || m.hosts == nil {
		return terminal.Snapshot{}, fmt.Errorf("session hosts are unavailable")
	}
	client, ok := m.hosts.Get(id)
	if !ok {
		return terminal.Snapshot{}, fmt.Errorf("session %s is not running", id)
	}
	var snapshot terminal.Snapshot
	if err := client.Call(context.Background(), "snapshot", nil, &snapshot); err != nil {
		return terminal.Snapshot{}, err
	}
	return snapshot, nil
}

// AttachAddr returns the socket a terminal attaches to for a managed session.
// Both providers serve the same attach surface, so one lookup covers them.
func (m *Manager) AttachAddr(id string) (string, error) {
	if m == nil || m.hosts == nil {
		return "", fmt.Errorf("session hosts are unavailable")
	}
	client, ok := m.hosts.Get(id)
	if !ok {
		return "", fmt.Errorf("session %s is not running", id)
	}
	if socket := client.AttachSocket(); socket != "" {
		return socket, nil
	}
	return "", fmt.Errorf("session %s has no attach socket", id)
}

func (m *Manager) isClosed() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.closed }

func (m *Manager) publish(coordinationID string, value event.Event) {
	m.mu.Lock()
	for ch := range m.subs[coordinationID] {
		select {
		case ch <- value:
		default:
		}
	}
	handler := m.eventHandler
	m.mu.Unlock()
	if handler != nil {
		handler(value)
	}
}

func (m *Manager) publishError(value session.Session, err error) {
	m.publish(value.CoordinationID, event.Event{ID: fmt.Sprintf("evt-%d", time.Now().UnixNano()), SessionID: value.ID, Kind: event.KindError, Source: event.SourceStream, Content: err.Error(), CreatedAt: time.Now().UTC()})
}
