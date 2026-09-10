package runtime

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

const (
	// resumeMatchWindow bounds how long a pending /resume trigger may wait for
	// confirming evidence. It must cover the whole interactive flow: opening the
	// provider session picker, searching, selecting a conversation, and typing
	// the next message. That routinely takes minutes, so a short window silently
	// drops real context switches. What prevents a wrong rebind is the evidence
	// rules below (workspace, post-trigger append, matching user text), not the
	// window length.
	resumeMatchWindow = 30 * time.Minute
	resumeGrace       = 2 * time.Second
	// resumeLineLimit caps how many submitted terminal lines are retained for
	// multi-line message matching.
	resumeLineLimit = 32
	// resumePollInterval is the stat cadence while waiting for the provider to
	// flush the confirming message.
	resumePollInterval = time.Second
)

type resumeBaseline struct {
	Path string
	Size int64
}

type resumePending struct {
	agent     string
	workspace string
	oldNative string
	triggered time.Time
	baseline  map[string]resumeBaseline
	expires   time.Time
	resolving bool
	// lines holds the terminal lines submitted after /resume. The provider may
	// persist them as one message (multi-line input or bracketed paste), so the
	// confirmation compares against single lines and consecutive joins.
	lines []string
}

// handleAgentInput is fed only submitted terminal lines. It deliberately
// treats /resume as a trigger, not as proof that the Agent changed context.
// The same state machine is used for every provider that exposes a history
// catalog and a managed terminal process.
func (m *Manager) handleAgentInput(id, content string) {
	text := normalizeRebindText(content)
	if text == "" || m.store == nil {
		return
	}
	value, err := m.store.GetSession(context.Background(), id)
	if err != nil {
		return
	}
	agent := strings.ToLower(strings.TrimSpace(value.Agent))
	if agent == "claude-code" {
		agent = "claude"
	}
	if agent != "pi" && agent != "claude" {
		return
	}
	if text == "/resume" || strings.HasPrefix(text, "/resume ") {
		m.beginResume(id, value, agent)
		return
	}

	m.mu.Lock()
	pending := m.resumePending[id]
	if pending == nil || pending.agent != agent || time.Now().After(pending.expires) {
		if pending != nil && time.Now().After(pending.expires) {
			delete(m.resumePending, id)
		}
		m.mu.Unlock()
		return
	}
	// Keep every line submitted since the trigger. The message the provider
	// persists is not necessarily the last single line: multi-line input and
	// bracketed paste arrive as several submitted lines but one user message.
	pending.lines = append(pending.lines, text)
	if len(pending.lines) > resumeLineLimit {
		pending.lines = pending.lines[len(pending.lines)-resumeLineLimit:]
	}
	// A resolver already polling picks the appended line up on its next pass.
	// Starting another one would only duplicate transcript scans.
	if pending.resolving {
		m.mu.Unlock()
		return
	}
	pending.resolving = true
	m.mu.Unlock()
	go m.tryResumeRebind(id, pending)
}

// clearResumePending drops the trigger state for a session that no longer
// exists, so a later session reusing the identity cannot inherit a stale
// baseline.
func (m *Manager) clearResumePending(id string) {
	m.mu.Lock()
	delete(m.resumePending, id)
	m.mu.Unlock()
}

func (m *Manager) beginResume(id string, value session.Session, agent string) {
	baseline := make(map[string]resumeBaseline)
	if agent == "pi" {
		values, err := adapter.NewPiHistoryCatalog(m.homeDir, m.piHistoryRoot()).List(context.Background())
		if err != nil {
			log.Printf("agora: %s /resume detection disabled for %s: history baseline unavailable: %v", agent, id, err)
			return
		}
		for _, item := range values {
			if item.Path != "" {
				baseline[item.Path] = resumeBaseline{Path: item.Path, Size: item.Size}
			}
		}
	} else {
		values, err := m.history.List(context.Background())
		if err != nil {
			log.Printf("agora: %s /resume detection disabled for %s: history baseline unavailable: %v", agent, id, err)
			return
		}
		for _, item := range values {
			if item.Path != "" {
				baseline[item.Path] = resumeBaseline{Path: item.Path, Size: item.Size}
			}
		}
	}
	m.mu.Lock()
	m.resumePending[id] = &resumePending{
		agent:     agent,
		workspace: value.Workspace,
		oldNative: strings.TrimPrefix(value.NativeSessionURI(), agent+"://"),
		triggered: time.Now().UTC(),
		baseline:  baseline,
		expires:   time.Now().Add(resumeMatchWindow),
	}
	m.mu.Unlock()
}

func (m *Manager) tryResumeRebind(id string, pending *resumePending) {
	defer func() {
		m.mu.Lock()
		if current := m.resumePending[id]; current == pending && current.resolving {
			current.resolving = false
		}
		m.mu.Unlock()
	}()

	deadline := pending.expires
	for time.Now().Before(deadline) {
		if m.isClosed() {
			return
		}
		m.mu.Lock()
		active := m.resumePending[id] == pending
		// Snapshot the submitted lines under the lock: handleAgentInput appends
		// to them while this loop polls for the provider's delayed flush.
		lines := append([]string(nil), pending.lines...)
		m.mu.Unlock()
		if !active {
			return
		}
		// Provider transcripts are append-only, so a candidate can only become
		// confirmable after its file grows past the /resume baseline. Stat the
		// small baseline set first and only pay for parsing when something grew;
		// otherwise the long wait window would re-scan every transcript
		// continuously.
		if grownResumeFiles(pending.baseline) == 0 {
			if !waitResumePoll(deadline) {
				return
			}
			continue
		}
		type candidate struct {
			path, native, workspace, name string
			baseSize                      int64
		}
		matches := make([]candidate, 0, 2)
		if pending.agent == "pi" {
			values, err := adapter.NewPiHistoryCatalog(m.homeDir, m.piHistoryRoot()).List(context.Background())
			if err == nil {
				for _, item := range values {
					base, ok := pending.baseline[item.Path]
					if ok && resumePiCandidate(item, base, pending, lines) {
						matches = append(matches, candidate{item.Path, item.SessionID, item.Workspace, item.SessionName, base.Size})
					}
				}
			}
		} else {
			values, err := m.history.List(context.Background())
			if err == nil {
				for _, item := range values {
					base, ok := pending.baseline[item.Path]
					if ok && resumeClaudeCandidate(item, base, pending, lines) {
						matches = append(matches, candidate{item.Path, item.SessionID, item.Workspace, item.LatestAITitle, base.Size})
					}
				}
			}
		}
		if len(matches) == 1 {
			target := matches[0]
			if err := m.rebindSession(id, pending.agent, target.native, target.path, target.workspace, target.name, target.baseSize); err == nil {
				m.mu.Lock()
				if m.resumePending[id] == pending {
					delete(m.resumePending, id)
				}
				m.mu.Unlock()
				return
			}
			return
		}
		// Zero matches can be caused by delayed provider flush. Multiple matches
		// remain ambiguous and must never select the first file arbitrarily.
		if !waitResumePoll(deadline) {
			return
		}
	}
}

// grownResumeFiles counts baseline transcripts that received new bytes since
// /resume. It is the cheap gate in front of the expensive history parsing.
func grownResumeFiles(baseline map[string]resumeBaseline) int {
	grown := 0
	for path, base := range baseline {
		info, err := os.Stat(path)
		if err != nil || info.Size() <= base.Size {
			continue
		}
		grown++
	}
	return grown
}

// waitResumePoll sleeps one poll interval and reports whether the caller may
// keep waiting. It returns false once the window is exhausted.
func waitResumePoll(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	interval := resumePollInterval
	if remaining < interval {
		interval = remaining
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	<-timer.C
	return time.Now().Before(deadline)
}

func resumePiCandidate(item adapter.PiHistorySummary, base resumeBaseline, pending *resumePending, lines []string) bool {
	if item.SessionID == "" || item.SessionID == pending.oldNative || item.Path == "" || item.Size <= base.Size {
		return false
	}
	if !sameRebindWorkspace(item.Workspace, pending.workspace) {
		return false
	}
	records, err := adapter.ReadPiHistory(context.Background(), adapter.PiHistoryCursor{Path: item.Path, ByteOffset: base.Size}, pending.oldNative)
	if err != nil {
		return false
	}
	return hasMatchingUser(recordsToEvents(records), pending, lines)
}

func resumeClaudeCandidate(item adapter.HistorySummary, base resumeBaseline, pending *resumePending, lines []string) bool {
	if item.SessionID == "" || item.SessionID == pending.oldNative || item.Path == "" || item.Size <= base.Size {
		return false
	}
	if !sameRebindWorkspace(item.Workspace, pending.workspace) {
		return false
	}
	records, err := adapter.ReadHistory(context.Background(), adapter.HistoryCursor{Path: item.Path, ByteOffset: base.Size}, pending.oldNative)
	if err != nil {
		return false
	}
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return hasMatchingUser(values, pending, lines)
}

func recordsToEvents(records []adapter.PiHistoryRecord) []event.Event {
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return values
}

func hasMatchingUser(values []event.Event, pending *resumePending, lines []string) bool {
	expected := submittedTexts(lines)
	if len(expected) == 0 {
		return false
	}
	now := time.Now()
	for _, item := range values {
		if item.Kind != event.KindUser {
			continue
		}
		// Only records appended after the trigger are evidence. Older records in
		// the same file must never confirm a context switch.
		if !item.CreatedAt.IsZero() && (item.CreatedAt.Before(pending.triggered.Add(-resumeGrace)) || item.CreatedAt.After(now.Add(resumeGrace))) {
			continue
		}
		if _, ok := expected[normalizeRebindText(item.Content)]; ok {
			return true
		}
	}
	return false
}

// submittedTexts expands the lines submitted since /resume into every user
// message they could represent. Single lines cover ordinary input; joins of
// consecutive lines cover multi-line messages and bracketed paste, which the
// provider stores as one user message but the terminal delivers as several
// submitted lines.
func submittedTexts(lines []string) map[string]struct{} {
	values := make(map[string]struct{})
	for length := 1; length <= len(lines); length++ {
		if text := normalizeRebindText(strings.Join(lines[len(lines)-length:], "\n")); text != "" {
			values[text] = struct{}{}
		}
	}
	return values
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
	if agent == "pi" && m.pi != nil {
		updated.ProcessID = old.ProcessID
	} else if agent == "claude" && m.pty != nil {
		updated.ProcessID = old.ProcessID
	}
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
					// The health watchdog and the input subscription are keyed by the
					// canonical id, so they must follow the rebind. Otherwise the Host
					// stops being monitored and later /resume triggers are invisible.
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
