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
	"unicode/utf8"

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
	// submitted line it is supposed to confirm. A provider writes the user
	// message as it is submitted, so a tight bound is both accurate and a strong
	// guard: text that merely resembles another session's message minutes apart
	// can never count as proof.
	switchEvidenceSkew = time.Minute
	// messageGramRunes is the phrase width used to compare an observed keystroke
	// line with a stored user message. The two streams are not identical: an
	// input method streams preedit text and replacement characters, and editing
	// arrives as backspaces, so the composer's final text differs from what the
	// terminal emitted. Matching measures how much of the observed phrase
	// reappears in the stored message instead of comparing bytes.
	messageGramRunes = 4
	// messageGramRatio is the share of observed phrases that must reappear.
	messageGramRatio = 0.5
	// messageGramFloor keeps very short messages from matching on one fragment.
	messageGramFloor = 2
)

// switchDebugf appends a diagnostic line when AGORA_SWITCH_DEBUG is set. The
// watcher runs inside the Daemon, whose stdout is the user's terminal, so the
// state machine writes to a file that can be inspected after the fact.
func switchDebugf(format string, args ...any) {
	if strings.TrimSpace(os.Getenv("AGORA_SWITCH_DEBUG")) == "" {
		return
	}
	path := strings.TrimSpace(os.Getenv("AGORA_SWITCH_DEBUG_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		path = filepath.Join(home, ".agora", "switch-debug.log")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

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
	id         string
	agent      string
	workspace  string
	ownNative  string
	ownPaths   map[string]bool
	candidates map[string]*switchCandidate
	lines      []watchedLine
	listedAt   time.Time
	cancel     context.CancelFunc
}

func newSwitchWatcher(id, agent, workspace, ownNative, ownPath string) *switchWatcher {
	watcher := &switchWatcher{id: id, agent: agent, workspace: workspace, ownNative: ownNative, ownPaths: make(map[string]bool), candidates: make(map[string]*switchCandidate)}
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

// lineCount reports how many submitted lines are retained. It is used by tests
// to wait until the Daemon observed terminal input.
func (w *switchWatcher) lineCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.lines)
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

// evidenceText is one user message the terminal input could represent, with the
// moment it was submitted.
type evidenceText struct {
	text string
	at   time.Time
}

// evidenceTexts lists every user message the submitted lines could represent.
// Joins of consecutive lines cover multi-line input and paste, which the
// provider stores as a single user message.
func (w *switchWatcher) evidenceTexts(now time.Time) []evidenceText {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := make([]watchedLine, 0, len(w.lines))
	for _, line := range w.lines {
		if now.Sub(line.at) > switchLineWindow {
			continue
		}
		lines = append(lines, line)
	}
	values := make([]evidenceText, 0, len(lines))
	for length := 1; length <= len(lines); length++ {
		window := lines[len(lines)-length:]
		texts := make([]string, 0, len(window))
		for _, line := range window {
			texts = append(texts, line.text)
		}
		if text := normalizeRebindText(strings.Join(texts, "\n")); text != "" {
			values = append(values, evidenceText{text: text, at: window[len(window)-1].at})
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
	watcher := newSwitchWatcher(value.ID, agent, value.Workspace, native, value.HistoryPath)
	switchDebugf("watcher start id=%s agent=%s workspace=%q own_native=%q own_path=%q", value.ID, agent, value.Workspace, native, value.HistoryPath)
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
	started := time.Now()
	entries, err := m.switchCatalog(ctx, watcher.agent)
	if err != nil {
		log.Printf("agora: context switch catalog unavailable: %v", err)
		switchDebugf("catalog id=%s error=%v", watcher.id, err)
		return
	}
	switchDebugf("catalog id=%s entries=%d took=%s", watcher.id, len(entries), time.Since(started).Round(time.Millisecond))
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
	grown := 0
	for _, candidate := range paths {
		info, err := os.Stat(candidate.path)
		if err != nil || info.Size() <= candidate.size {
			continue
		}
		grown++
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
		matched, consumed := m.readSwitchIncrement(ctx, watcher.agent, *candidate, from, texts)
		switchDebugf("scan id=%s path=%s from=%d to=%d consumed=%d matched=%v", watcher.id, candidate.path, from, info.Size(), consumed, matched)
		if matched {
			matches = append(matches, *candidate)
		}
		// Never move the cursor past a partially written record: the provider
		// appends a record in pieces, and skipping the tail would drop the very
		// message that confirms the switch.
		watcher.advance(candidate.path, consumed)
	}
	if grown > 0 {
		switchDebugf("scan id=%s texts=%d grown=%d matches=%d", watcher.id, len(texts), grown, len(matches))
	}
	if len(matches) != 1 {
		return false
	}
	target := matches[0]
	if target.sessionID == "" {
		return false
	}
	if _, err := m.rebindSession(id, watcher.agent, target.sessionID, target.path, target.workspace, "", target.size); err != nil {
		log.Printf("agora: context switch to %s could not be applied: %v", target.sessionID, err)
		switchDebugf("rebind id=%s target=%s error=%v", id, target.sessionID, err)
		return false
	}
	log.Printf("agora: session %s followed a provider context switch to %s", id, target.sessionID)
	switchDebugf("rebind id=%s target=%s history=%s ok", id, target.sessionID, target.path)
	return true
}

func (w *switchWatcher) advance(path string, size int64) {
	w.mu.Lock()
	if candidate := w.candidates[path]; candidate != nil && size > candidate.size {
		candidate.size = size
	}
	w.mu.Unlock()
}

// readSwitchIncrement parses the bytes appended after the cursor and reports
// whether they confirm one of the submitted lines, together with the offset up
// to which complete records were consumed.
//
// The second value is the important one. Providers append a JSONL record in
// pieces, so a scan routinely observes a half-written line. Advancing the
// cursor to the current file size would silently discard that line, and the
// confirming message would never be seen again.
func (m *Manager) readSwitchIncrement(ctx context.Context, agent string, candidate switchCandidate, from int64, texts []evidenceText) (bool, int64) {
	if agent == "pi" {
		records, err := adapter.ReadPiHistory(ctx, adapter.PiHistoryCursor{Path: candidate.path, ByteOffset: from}, "pi://"+candidate.sessionID)
		if err != nil {
			return false, from
		}
		values := make([]event.Event, 0, len(records))
		consumed := from
		for _, record := range records {
			values = append(values, record.Event)
			if !recordOutsideEvidenceWindow(record.Event) {
				// Records inside the evidence window stay eligible: a scan can
				// observe them before the confirming line (or a better matching
				// rule) is available, and consuming them would lose the switch.
				continue
			}
			if record.Cursor.ByteOffset > consumed {
				consumed = record.Cursor.ByteOffset
			}
		}
		return hasMatchingUser(values, texts), consumed
	}
	records, err := adapter.ReadHistory(ctx, adapter.HistoryCursor{Path: candidate.path, ByteOffset: from}, candidate.sessionID)
	if err != nil {
		return false, from
	}
	values := make([]event.Event, 0, len(records))
	consumed := from
	for _, record := range records {
		values = append(values, record.Event)
		if !recordOutsideEvidenceWindow(record.Event) {
			continue
		}
		if record.Cursor.ByteOffset > consumed {
			consumed = record.Cursor.ByteOffset
		}
	}
	return hasMatchingUser(values, texts), consumed
}

// recordOutsideEvidenceWindow reports whether a record is old enough to be
// dropped from the candidate window.
func recordOutsideEvidenceWindow(item event.Event) bool {
	return !item.CreatedAt.IsZero() && time.Since(item.CreatedAt) > switchLineWindow
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
		values, err := m.piCatalog().List(ctx)
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
		// A Session Host is registered under its canonical id, which changes on
		// rebind. Resolve the watcher from the Host's own identity as a fallback
		// so input observation cannot be lost to a registry key mismatch.
		watcher = m.switchWatcherByNative(id, text)
	}
	if watcher == nil {
		switchDebugf("input id=%s no watcher for line=%q", id, truncateDebug(text))
		return
	}
	switchDebugf("input id=%s watcher=%s line=%q", id, watcher.id, truncateDebug(text))
	watcher.recordLine(text, time.Now())
}

// switchWatcherByNative finds a watcher that owns the given native session id.
// It is only used when the canonical id lookup misses.
func (m *Manager) switchWatcherByNative(id, _ string) *switchWatcher {
	value, err := m.store.GetSession(context.Background(), id)
	if err != nil {
		return nil
	}
	native := strings.TrimPrefix(value.NativeSessionURI(), value.Agent+"://")
	if native == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, watcher := range m.switchWatchers {
		if watcher.ownNative == native {
			return watcher
		}
	}
	return nil
}

func truncateDebug(value string) string {
	if len(value) <= 60 {
		return value
	}
	return value[:60] + "…"
}

// hasMatchingUser reports whether the transcript carries a user message that
// corresponds to one of the submitted lines.
//
// The comparison is deliberately tolerant. The terminal stream contains input
// method preedit text, replacement characters and edits, while the provider
// stores only the composer's final text, so an observed line and its stored
// message are rarely byte-identical. A long shared phrase is required, and the
// caller additionally requires the same workspace, a submission within a minute
// and a unique candidate.
func hasMatchingUser(values []event.Event, expected []evidenceText) bool {
	if len(expected) == 0 {
		return false
	}
	for _, item := range values {
		if item.Kind != event.KindUser {
			continue
		}
		stored := normalizeRebindText(item.Content)
		if stored == "" {
			continue
		}
		for _, candidate := range expected {
			if !item.CreatedAt.IsZero() {
				if diff := item.CreatedAt.Sub(candidate.at); diff > switchEvidenceSkew || diff < -switchEvidenceSkew {
					continue
				}
			}
			if sameUserMessage(candidate.text, stored) {
				return true
			}
		}
	}
	return false
}

// sameUserMessage reports whether an observed keystroke line and a stored user
// message are the same message. It compares shared phrases rather than bytes so
// that input-method noise and editing do not hide a real switch, while a merely
// similar message still fails.
func sameUserMessage(observed, stored string) bool {
	left := normalizeRebindText(sanitizeObservedText(observed))
	right := normalizeRebindText(stored)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	observedGrams := phraseGrams(left)
	if len(observedGrams) == 0 {
		return false
	}
	storedGrams := phraseGrams(right)
	shared := 0
	for gram := range observedGrams {
		if _, ok := storedGrams[gram]; ok {
			shared++
		}
	}
	need := int(float64(len(observedGrams)) * messageGramRatio)
	if float64(need) < float64(len(observedGrams))*messageGramRatio {
		need++
	}
	if need < messageGramFloor {
		need = messageGramFloor
	}
	return shared >= need
}

// sanitizeObservedText drops the replacement characters a terminal or input
// method emits for input it cannot represent. They carry no user intent and
// would otherwise break every phrase that spans them.
func sanitizeObservedText(value string) string {
	if !strings.ContainsRune(value, utf8.RuneError) {
		return value
	}
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError {
			return -1
		}
		return r
	}, value)
}

// phraseGrams returns the overlapping rune windows of a normalized text.
func phraseGrams(text string) map[string]struct{} {
	runes := []rune(text)
	if len(runes) < messageGramRunes {
		return nil
	}
	grams := make(map[string]struct{}, len(runes)-messageGramRunes+1)
	for index := 0; index+messageGramRunes <= len(runes); index++ {
		grams[string(runes[index:index+messageGramRunes])] = struct{}{}
	}
	return grams
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

func (m *Manager) rebindSession(id, agent, nativeID, historyPath, workspace, displayName string, baseSize int64) (session.Session, error) {
	old, err := m.store.GetSession(context.Background(), id)
	if err != nil {
		return session.Session{}, err
	}
	if nativeID == "" {
		// A fresh provider session is bound by id before its transcript exists;
		// the observer fills HistoryPath once the file appears.
		return session.Session{}, fmt.Errorf("%s rebind target is incomplete", agent)
	}
	if workspace != "" && old.Workspace != "" && !sameRebindWorkspace(workspace, old.Workspace) {
		return session.Session{}, fmt.Errorf("%s rebind target workspace does not match", agent)
	}
	newID := id
	if m.daemonID != "" {
		candidate, identityErr := session.NewSessionID(m.daemonID, agent, agent+"://"+nativeID)
		if identityErr != nil {
			return session.Session{}, identityErr
		}
		newID = candidate
	}
	// /resume often targets a previously discovered history row that already
	// uses this canonical id. Replace that row instead of failing: the live
	// process is moving onto the same logical session.

	updated := old
	updated.ID = newID
	updated.Agent = agent
	updated.AgentSessionID = agent + "://" + nativeID
	if agent == "claude" {
		updated.ClaudeSessionID = nativeID
	}
	updated.HistoryPath = historyPath
	updated.Capabilities = managedHostCapabilities()
	if agent == "pi" {
		updated.Capabilities = piCapabilities()
	}
	applyManagedHistoryCapability(&updated)
	updated.Workspace = firstNonEmpty(old.Workspace, workspace)
	updated.State = session.StateRunning
	updated.Connection = session.ConnectionObserved
	// The existing managed process remains alive; rebind must not make the
	// server believe the PTY exited while only its logical context changed.
	updated.ProcessID = old.ProcessID
	if existing, getErr := m.store.GetSession(context.Background(), newID); getErr == nil && existing.ID == newID {
		// Keep the target session's own name when it already has one. A
		// /resume onto a discovered history row would otherwise overwrite
		// "older conversation" with the live wrapper's "New session".
		if !session.IsGeneratedDisplayName(existing.DisplayName) {
			updated.DisplayName = existing.DisplayName
			updated.DisplayNameSource = existing.DisplayNameSource
		}
	}
	if displayName != "" && (session.IsGeneratedDisplayName(updated.DisplayName) || updated.DisplayNameSource == session.DisplayNameSourceAITitle) {
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
			return session.Session{}, err
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
			return session.Session{}, managerErr
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
			return session.Session{}, managerErr
		}
		if err := m.store.UpdateSessionObservation(context.Background(), updated); err != nil {
			return session.Session{}, err
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
			return session.Session{}, err
		}
	} else if err := m.StartObserver(updated); err != nil {
		return session.Session{}, err
	}
	m.mu.Lock()
	handler := m.sessionRebind
	m.mu.Unlock()
	if handler != nil && id != updated.ID {
		handler(id, updated)
	}
	return updated, nil
}

// AgentSessionReport is a provider-native session report from the injected Agent
// extension. It is authoritative: the provider says which session the Agent is
// using, so no keystroke or transcript inference is involved.
type AgentSessionReport struct {
	HostID              string
	Reason              string
	SessionFile         string
	TargetSessionFile   string
	PreviousSessionFile string
	NativeSessionID     string
	SessionName         string
}

// ReportAgentSession applies an Agent session report and rebinds the managed
// session when the provider moved to a different one.
func (m *Manager) ReportAgentSession(ctx context.Context, report AgentSessionReport) (session.Session, error) {
	if m == nil || m.hosts == nil {
		return session.Session{}, fmt.Errorf("session hosts are unavailable")
	}
	id, ok := m.sessionIDForHost(report.HostID)
	if !ok {
		return session.Session{}, fmt.Errorf("no managed session for host %s", report.HostID)
	}
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, fmt.Errorf("resolve managed session %s for host %s: %w", id, report.HostID, err)
	}
	agent := normalizeSwitchAgent(value.Agent)
	if agent != "pi" {
		// Only Pi reports through the extension today; other providers keep the
		// evidence-based path.
		return value, nil
	}

	file := strings.TrimSpace(report.SessionFile)
	if file == "" {
		file = strings.TrimSpace(report.TargetSessionFile)
	}
	native := strings.TrimSpace(report.NativeSessionID)
	if native == "" {
		native = piNativeIDFromSessionFile(file)
	}
	if native == "" {
		return session.Session{}, fmt.Errorf("session report for host %s carries no session identity", report.HostID)
	}
	if resolved := adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), native); file != "" && resolved != "" && resolved != file {
		return session.Session{}, fmt.Errorf("session report file %q does not belong to session %s", file, native)
	}

	current := strings.TrimPrefix(value.NativeSessionURI(), "pi://")
	if current == native && value.HistoryPath == file {
		return value, nil
	}
	switchDebugf("report id=%s reason=%s native=%s file=%s", id, report.Reason, native, file)
	// /new names the future JSONL before it exists. Bind the native id now, but
	// leave HistoryPath empty until the file appears so the UI does not request
	// a transcript that is not there yet.
	if file != "" {
		if _, statErr := os.Stat(file); os.IsNotExist(statErr) {
			file = ""
		}
	}
	// Start the observer at the end of an existing transcript: the user already
	// has that history, and replaying it would emit it as live events.
	updated, err := m.rebindSession(id, agent, native, file, value.Workspace, report.SessionName, -1)
	if err != nil {
		switchDebugf("report id=%s native=%s failed: %v", id, native, err)
		return value, err
	}
	log.Printf("agora: session %s reported a provider switch to %s", id, native)
	return updated, nil
}

// sessionIDForHost resolves the canonical Session ID a Host currently owns. The
// registry is keyed by Session ID, and Host IDs survive rebinds.
func (m *Manager) sessionIDForHost(hostID string) (string, bool) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" || m.hosts == nil {
		return "", false
	}
	for id, client := range m.hosts.Clients() {
		if client == nil {
			continue
		}
		if client.Metadata().HostID == hostID {
			return id, true
		}
	}
	return "", false
}

// piNativeIDFromSessionFile extracts the session id from a Pi transcript name,
// which the provider writes as "<timestamp>_<session-id>.jsonl".
func piNativeIDFromSessionFile(path string) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSpace(path)), ".jsonl")
	if base == "" {
		return ""
	}
	if index := strings.LastIndexByte(base, '_'); index >= 0 {
		base = base[index+1:]
	}
	return strings.TrimSpace(base)
}
