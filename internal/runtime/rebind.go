package runtime

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

const (
	// switchScanInterval is the stat cadence for candidate transcripts. Provider
	// history is append-only, so between scans only a cheap stat is needed.
	switchScanInterval = 2 * time.Second
	// switchCatalogInterval bounds how often the full provider catalog is
	// re-listed to discover transcripts created after the watcher started.
	switchCatalogInterval = 60 * time.Second
	// switchLineWindow is how long a submitted line stays eligible as evidence.
	// A context switch can only be confirmed by a message the user actually
	// sent, but the provider may flush it long after the keystroke.
	switchLineWindow = 30 * time.Minute
	// switchLineLimit bounds retained submitted lines.
	switchLineLimit = 32
	// switchIncrementCap bounds how many bytes of a candidate transcript are
	// parsed per scan.
	switchIncrementCap = 1 << 20
	// switchCatalogTTL shares one provider catalog snapshot between watchers.
	// Listing a large provider history means parsing every transcript, so it
	// must not happen once per managed session.
	switchCatalogTTL = 15 * time.Second
	// switchEvidenceSkew bounds how far a provider record may sit from the
	// submitted line it is supposed to confirm. The provider writes the user
	// message when it is submitted, so a wide skew is unnecessary; a tight one
	// keeps identical text from an unrelated session in the same workspace from
	// ever counting as proof.
	switchEvidenceSkew = 5 * time.Minute
)

// switchCandidate tracks one transcript file that could belong to a session.
type switchCandidate struct {
	path      string
	sessionID string
	workspace string
	size      int64
}

type watchedLine struct {
	text string
	at   time.Time
}

// switchWatcher detects a provider context switch (/resume, /fork, a resume
// picker, or starting pi directly into an old session) by evidence instead of
// by keystroke. Relying on the literal "/resume" text is not reliable: the TUI
// may submit a command chosen from a completion menu, the user may reach the
// picker another way, and the wrapper forwards the provider's own arguments.
//
// The watcher therefore compares what the user actually typed with messages the
// provider wrote into a transcript that is NOT this session's transcript.
//
//	submitted line  +  new user record in another transcript of the same
//	workspace  =>  the Agent switched context
//
// A unique candidate is required, so an ambiguous situation never rebinds.
type switchWatcher struct {
	mu         sync.Mutex
	agent      string
	workspace  string
	ownNative  string
	ownPaths   map[string]bool
	candidates map[string]*switchCandidate
	lines      []watchedLine
	listedAt   time.Time
	cancel     context.CancelFunc
}

func newSwitchWatcher(agent, workspace, ownNative, ownPath string) *switchWatcher {
	watcher := &switchWatcher{agent: agent, workspace: workspace, ownNative: ownNative, ownPaths: make(map[string]bool), candidates: make(map[string]*switchCandidate)}
	if ownPath != "" {
		watcher.ownPaths[ownPath] = true
	}
	return watcher
}

// knownCandidate reports whether the watcher already tracks a transcript. It is
// used by tests to wait for the initial catalog snapshot.
func (w *switchWatcher) knownCandidate(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.candidates[path] != nil
}

func (w *switchWatcher) recordLine(text string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, watchedLine{text: text, at: at})
	if len(w.lines) > switchLineLimit {
		w.lines = w.lines[len(w.lines)-switchLineLimit:]
	}
}

// ownedBySession reports whether a transcript already belongs to the session
// the watcher guards, in which case it can never be switch evidence.
func (w *switchWatcher) ownedBySession(candidate switchCandidate) bool {
	if w.ownPaths[candidate.path] {
		return true
	}
	return candidate.sessionID != "" && candidate.sessionID == w.ownNative
}

// evidenceTexts maps every user message the submitted lines could represent to
// the moment it was submitted. Joins of consecutive lines cover multi-line
// input and paste, which the provider stores as a single user message.
func (w *switchWatcher) evidenceTexts(now time.Time) map[string]time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := make([]watchedLine, 0, len(w.lines))
	for _, line := range w.lines {
		if now.Sub(line.at) > switchLineWindow {
			continue
		}
		lines = append(lines, line)
	}
	values := make(map[string]time.Time)
	for length := 1; length <= len(lines); length++ {
		window := lines[len(lines)-length:]
		texts := make([]string, 0, len(window))
		for _, line := range window {
			texts = append(texts, line.text)
		}
		if text := normalizeRebindText(strings.Join(texts, "\n")); text != "" {
			values[text] = window[len(window)-1].at
		}
	}
	return values
}

func (m *Manager) startSwitchWatcher(value session.Session) {
	if m == nil || m.store == nil || m.closed {
		return
	}
	agent := normalizeSwitchAgent(value.Agent)
	if agent == "" || value.Source != session.SourceManaged {
		return
	}
	native := strings.TrimPrefix(value.NativeSessionURI(), agent+"://")
	watcher := newSwitchWatcher(agent, value.Workspace, native, value.HistoryPath)
	ctx, cancel := context.WithCancel(context.Background())
	watcher.cancel = cancel
	m.mu.Lock()
	m.stopSwitchWatcherLocked(value.ID)
	m.switchWatchers[value.ID] = watcher
	m.mu.Unlock()
	go m.watchContextSwitch(ctx, value.ID, watcher)
}

func (m *Manager) stopSwitchWatcher(id string) {
	m.mu.Lock()
	m.stopSwitchWatcherLocked(id)
	m.mu.Unlock()
}

// stopSwitchWatcherLocked requires m.mu to be held.
func (m *Manager) stopSwitchWatcherLocked(id string) {
	watcher := m.switchWatchers[id]
	delete(m.switchWatchers, id)
	if watcher != nil && watcher.cancel != nil {
		watcher.cancel()
	}
}

func normalizeSwitchAgent(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "pi":
		return "pi"
	case "claude", "claude-code":
		return "claude"
	default:
		return ""
	}
}

// watchContextSwitch keeps the watcher's candidate set fresh and scans only the
// bytes appended since the previous scan.
func (m *Manager) watchContextSwitch(ctx context.Context, id string, watcher *switchWatcher) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if m.isClosed() {
			return
		}
		m.refreshSwitchCandidates(ctx, watcher)
		if m.scanSwitchCandidates(ctx, id, watcher) {
			return
		}
		timer := time.NewTimer(switchScanInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) refreshSwitchCandidates(ctx context.Context, watcher *switchWatcher) {
	watcher.mu.Lock()
	listedAt := watcher.listedAt
	watcher.mu.Unlock()
	if !listedAt.IsZero() && time.Since(listedAt) < switchCatalogInterval {
		return
	}
	entries, err := m.switchCatalog(ctx, watcher.agent)
	if err != nil {
		log.Printf("agora: context switch catalog unavailable: %v", err)
		return
	}
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	watcher.listedAt = time.Now()
	for _, entry := range entries {
		if entry.path == "" || watcher.ownedBySessionLocked(entry) {
			continue
		}
		if existing, ok := watcher.candidates[entry.path]; ok {
			// The provider may rewrite metadata (workspace, name) while the
			// transcript keeps growing; keep the freshest values.
			existing.workspace = firstNonEmpty(entry.workspace, existing.workspace)
			existing.sessionID = firstNonEmpty(entry.sessionID, existing.sessionID)
			continue
		}
		candidate := entry
		watcher.candidates[entry.path] = &candidate
	}
}

func (w *switchWatcher) ownedBySessionLocked(candidate switchCandidate) bool {
	if w.ownPaths[candidate.path] {
		return true
	}
	return candidate.sessionID != "" && candidate.sessionID == w.ownNative
}

func (m *Manager) scanSwitchCandidates(ctx context.Context, id string, watcher *switchWatcher) bool {
	texts := watcher.evidenceTexts(time.Now())
	if len(texts) == 0 {
		return false
	}
	watcher.mu.Lock()
	paths := make([]*switchCandidate, 0, len(watcher.candidates))
	for _, candidate := range watcher.candidates {
		paths = append(paths, candidate)
	}
	watcher.mu.Unlock()

	matches := make([]switchCandidate, 0, 2)
	for _, candidate := range paths {
		info, err := os.Stat(candidate.path)
		if err != nil || info.Size() <= candidate.size {
			continue
		}
		if candidate.workspace != "" && watcher.workspace != "" && !sameRebindWorkspace(candidate.workspace, watcher.workspace) {
			// Keep the cursor moving so an unrelated project is not rescanned
			// from the beginning on every cycle.
			watcher.advance(candidate.path, info.Size())
			continue
		}
		from := candidate.size
		if info.Size()-from > switchIncrementCap {
			from = info.Size() - switchIncrementCap
		}
		if m.switchIncrementHasUser(ctx, watcher.agent, *candidate, from, texts) {
			matches = append(matches, *candidate)
		}
		watcher.advance(candidate.path, info.Size())
	}
	if len(matches) != 1 {
		return false
	}
	target := matches[0]
	if target.sessionID == "" {
		return false
	}
	if err := m.rebindSession(id, watcher.agent, target.sessionID, target.path, target.workspace, "", target.size); err != nil {
		log.Printf("agora: context switch to %s could not be applied: %v", target.sessionID, err)
		return false
	}
	log.Printf("agora: session %s followed a provider context switch to %s", id, target.sessionID)
	return true
}

func (w *switchWatcher) advance(path string, size int64) {
	w.mu.Lock()
	if candidate := w.candidates[path]; candidate != nil && size > candidate.size {
		candidate.size = size
	}
	w.mu.Unlock()
}

// switchIncrementHasUser reports whether the transcript's appended bytes carry
// a user message matching one of the submitted lines.
func (m *Manager) switchIncrementHasUser(ctx context.Context, agent string, candidate switchCandidate, from int64, texts map[string]time.Time) bool {
	if agent == "pi" {
		records, err := adapter.ReadPiHistory(ctx, adapter.PiHistoryCursor{Path: candidate.path, ByteOffset: from}, "pi://"+candidate.sessionID)
		if err != nil {
			return false
		}
		values := make([]event.Event, 0, len(records))
		for _, record := range records {
			values = append(values, record.Event)
		}
		return hasMatchingUser(values, texts)
	}
	records, err := adapter.ReadHistory(ctx, adapter.HistoryCursor{Path: candidate.path, ByteOffset: from}, candidate.sessionID)
	if err != nil {
		return false
	}
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return hasMatchingUser(values, texts)
}

type switchCatalogCache struct {
	at      time.Time
	entries []switchCandidate
	err     error
}

// switchCatalog snapshots every transcript the provider knows about. The
// snapshot is shared between watchers for a short period because building it
// parses the whole provider history.
func (m *Manager) switchCatalog(ctx context.Context, agent string) ([]switchCandidate, error) {
	m.mu.Lock()
	cached, ok := m.switchCache[agent]
	m.mu.Unlock()
	if ok && time.Since(cached.at) < switchCatalogTTL {
		return cached.entries, cached.err
	}
	entries, err := m.listSwitchCatalog(ctx, agent)
	m.mu.Lock()
	if m.switchCache == nil {
		m.switchCache = make(map[string]switchCatalogCache)
	}
	m.switchCache[agent] = switchCatalogCache{at: time.Now(), entries: entries, err: err}
	m.mu.Unlock()
	return entries, err
}

func (m *Manager) listSwitchCatalog(ctx context.Context, agent string) ([]switchCandidate, error) {
	if agent == "pi" {
		values, err := adapter.NewPiHistoryCatalog(m.homeDir, m.piHistoryRoot()).List(ctx)
		if err != nil {
			return nil, err
		}
		entries := make([]switchCandidate, 0, len(values))
		for _, item := range values {
			if item.Path == "" || item.SessionID == "" {
				continue
			}
			entries = append(entries, switchCandidate{path: item.Path, sessionID: item.SessionID, workspace: item.Workspace, size: item.Size})
		}
		return entries, nil
	}
	values, err := m.history.List(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]switchCandidate, 0, len(values))
	for _, item := range values {
		if item.Path == "" || item.SessionID == "" {
			continue
		}
		entries = append(entries, switchCandidate{path: item.Path, sessionID: item.SessionID, workspace: item.Workspace, size: item.Size})
	}
	return entries, nil
}

// handleAgentInput is fed only submitted terminal lines. Agora never infers a
// context switch from the line itself; the line becomes evidence only when the
// provider writes the same user message into a different transcript.
func (m *Manager) handleAgentInput(id, content string) {
	text := normalizeRebindText(content)
	if text == "" || m.store == nil {
		return
	}
	m.mu.Lock()
	watcher := m.switchWatchers[id]
	m.mu.Unlock()
	if watcher == nil {
		return
	}
	watcher.recordLine(text, time.Now())
}

// hasMatchingUser reports whether the provider wrote one of the submitted
// messages into the transcript. The record must carry the same text and must
// have been written around the moment the line was submitted, so identical text
// from an unrelated point in time can never confirm a context switch.
func hasMatchingUser(values []event.Event, expected map[string]time.Time) bool {
	if len(expected) == 0 {
		return false
	}
	for _, item := range values {
		if item.Kind != event.KindUser {
			continue
		}
		submittedAt, ok := expected[normalizeRebindText(item.Content)]
		if !ok {
			continue
		}
		if item.CreatedAt.IsZero() {
			return true
		}
		if diff := item.CreatedAt.Sub(submittedAt); diff <= switchEvidenceSkew && diff >= -switchEvidenceSkew {
			return true
		}
	}
	return false
}

func sameRebindWorkspace(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func normalizeRebindText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func (m *Manager) rebindSession(id, agent, nativeID, historyPath, workspace, displayName string, baseSize int64) error {
	old, err := m.store.GetSession(context.Background(), id)
	if err != nil {
		return err
	}
	if nativeID == "" || historyPath == "" {
		return fmt.Errorf("%s rebind target is incomplete", agent)
	}
	if workspace != "" && old.Workspace != "" && !sameRebindWorkspace(workspace, old.Workspace) {
		return fmt.Errorf("%s rebind target workspace does not match", agent)
	}
	newID := id
	if m.daemonID != "" {
		candidate, identityErr := session.NewSessionID(m.daemonID, agent, agent+"://"+nativeID)
		if identityErr != nil {
			return identityErr
		}
		newID = candidate
	}
	if newID != id {
		if _, getErr := m.store.GetSession(context.Background(), newID); getErr == nil {
			return fmt.Errorf("session %s already exists", newID)
		}
	}

	updated := old
	updated.ID = newID
	updated.Agent = agent
	updated.AgentSessionID = agent + "://" + nativeID
	if agent == "claude" {
		updated.ClaudeSessionID = nativeID
	}
	updated.HistoryPath = historyPath
	updated.Workspace = firstNonEmpty(old.Workspace, workspace)
	updated.State = session.StateRunning
	updated.Connection = session.ConnectionObserved
	// The existing managed process remains alive; rebind must not make the
	// server believe the PTY exited while only its logical context changed.
	updated.ProcessID = old.ProcessID
	if displayName != "" {
		updated.DisplayName = displayName
		updated.DisplayNameSource = session.DisplayNameSourceAITitle
	}

	if agent == "pi" {
		m.StopPiObserver(id)
	} else {
		m.StopObserver(id)
	}
	if newID != id {
		if err := m.store.RekeySession(context.Background(), id, updated); err != nil {
			return err
		}
		var managerErr error
		if m.hosts != nil {
			if client, ok := m.hosts.Get(id); ok {
				_, managerErr = client.Rebind(context.Background(), newID, updated.AgentSessionID, historyPath, updated.DisplayName)
				if managerErr == nil {
					m.hosts.Delete(id)
					m.hosts.Put(newID, client)
					// The health watchdog and the input subscription are keyed by
					// the canonical id, so they must follow the rebind.
					m.stopHostMonitor(id)
					m.monitorHost(newID)
				}
			} else {
				managerErr = fmt.Errorf("managed session %s has no session-host", id)
			}
		} else if agent == "pi" && m.pi != nil {
			managerErr = m.pi.Rebind(id, newID, nativeID)
		} else if agent == "claude" && m.pty != nil {
			managerErr = m.pty.Rebind(id, newID, nativeID)
		}
		if managerErr != nil {
			_ = m.store.RekeySession(context.Background(), newID, old)
			return managerErr
		}
		m.mu.Lock()
		if active := m.active[id]; active {
			m.active[newID] = active
			delete(m.active, id)
		}
		m.generation[newID] = m.generation[id]
		delete(m.generation, id)
		m.mu.Unlock()
		m.stopSwitchWatcher(id)
	} else {
		var managerErr error
		if m.hosts != nil {
			if client, ok := m.hosts.Get(id); ok {
				_, managerErr = client.Rebind(context.Background(), id, updated.AgentSessionID, historyPath, updated.DisplayName)
			} else {
				managerErr = fmt.Errorf("managed session %s has no session-host", id)
			}
		} else if agent == "pi" && m.pi != nil {
			managerErr = m.pi.Rebind(id, newID, nativeID)
		} else if agent == "claude" && m.pty != nil {
			managerErr = m.pty.Rebind(id, newID, nativeID)
		}
		if managerErr != nil {
			return managerErr
		}
		if err := m.store.UpdateSessionObservation(context.Background(), updated); err != nil {
			return err
		}
		m.stopSwitchWatcher(id)
	}
	if info, statErr := os.Stat(historyPath); statErr == nil {
		if baseSize < 0 || baseSize > info.Size() {
			baseSize = info.Size()
		}
		_ = m.store.SaveObservationCursor(context.Background(), store.ObservationCursor{SessionID: newID, Path: historyPath, ByteOffset: baseSize})
	}
	if agent == "pi" {
		if err := m.StartPiObserver(updated); err != nil {
			return err
		}
	} else if err := m.StartObserver(updated); err != nil {
		return err
	}
	m.mu.Lock()
	handler := m.sessionRebind
	m.mu.Unlock()
	if handler != nil && id != updated.ID {
		handler(id, updated)
	}
	return nil
}
