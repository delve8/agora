package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
)

func appendPiUserRecord(t *testing.T, path, sessionID, content string, at time.Time) {
	t.Helper()
	record := map[string]any{
		"type":      "message",
		"id":        fmt.Sprintf("rec-%d", at.UnixNano()),
		"sessionId": sessionID,
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": content}},
		},
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(body, '\n')); err != nil {
		t.Fatal(err)
	}
}

func piEventsAfter(t *testing.T, path string, from int64) []event.Event {
	t.Helper()
	records, err := adapter.ReadPiHistory(context.Background(), adapter.PiHistoryCursor{Path: path, ByteOffset: from}, "pi://picked")
	if err != nil {
		t.Fatal(err)
	}
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return values
}

// The keystroke itself is never proof. Only a user message the provider wrote
// into another transcript confirms a context switch.
func TestSwitchIncrementRequiresSubmittedLineInAnotherTranscript(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	header := fmt.Sprintf(`{"type":"session","id":"picked","cwd":%q}`, workspace) + "\n"
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	appendPiUserRecord(t, path, "picked", "an older question", time.Now().Add(-time.Hour))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := info.Size()

	manager := &Manager{}
	candidate := switchCandidate{path: path, sessionID: "picked", workspace: workspace, size: baseline}

	if manager.switchIncrementHasUser(context.Background(), "pi", candidate, baseline, map[string]time.Time{"new question": time.Now()}) {
		t.Fatal("matched before the provider appended anything")
	}

	appendPiUserRecord(t, path, "picked", "新会话里的问题", time.Now())

	// A line the user never typed must not confirm a switch.
	if manager.switchIncrementHasUser(context.Background(), "pi", candidate, baseline, map[string]time.Time{"some other text": time.Now()}) {
		t.Fatal("matched a message the user did not submit")
	}
	if !manager.switchIncrementHasUser(context.Background(), "pi", candidate, baseline, map[string]time.Time{"新会话里的问题": time.Now()}) {
		t.Fatal("did not match the appended user message")
	}

	// Records far from the submitted line are not proof, so a transcript that
	// already contains identical text can never confirm a switch by itself.
	all := piEventsAfter(t, path, 0)
	if hasMatchingUser(all, map[string]time.Time{"an older question": time.Now()}) {
		t.Fatal("a pre-existing transcript record confirmed a switch")
	}
	if hasMatchingUser(all, map[string]time.Time{"新会话里的问题": time.Now().Add(-time.Hour)}) {
		t.Fatal("a record outside the evidence skew confirmed a switch")
	}
	if !hasMatchingUser(all, map[string]time.Time{"新会话里的问题": time.Now()}) {
		t.Fatal("a freshly appended record was not accepted as evidence")
	}
}

// Multi-line input and bracketed paste reach the PTY as several submitted lines
// but the provider stores them as one user message.
func TestSwitchWatcherEvidenceCoversMultiLineInput(t *testing.T) {
	watcher := newSwitchWatcher("id", "pi", "/tmp/workspace", "own-native", "")
	watcher.recordLine("first line", time.Now())
	watcher.recordLine("second line", time.Now())
	texts := watcher.evidenceTexts(time.Now())
	for _, want := range []string{"first line second line", "second line"} {
		if _, ok := texts[want]; !ok {
			t.Fatalf("evidence is missing %q: %v", want, texts)
		}
	}
	if _, ok := texts["first line"]; ok {
		t.Fatalf("a prefix of older lines must not be evidence: %v", texts)
	}

	watcher.recordLine("too old", time.Now().Add(-2*switchLineWindow))
	if _, ok := watcher.evidenceTexts(time.Now())["too old"]; ok {
		t.Fatal("an expired line is still treated as evidence")
	}
}

// The session's own transcript can never be the switch target.
func TestSwitchWatcherIgnoresOwnTranscript(t *testing.T) {
	own := filepath.Join(t.TempDir(), "own.jsonl")
	watcher := newSwitchWatcher("id", "pi", "/tmp/workspace", "own-native", own)
	if !watcher.ownedBySession(switchCandidate{path: own, sessionID: "picked"}) {
		t.Fatal("the session's own history path was treated as a switch candidate")
	}
	if !watcher.ownedBySession(switchCandidate{path: "/elsewhere.jsonl", sessionID: "own-native"}) {
		t.Fatal("the session's own native id was treated as a switch candidate")
	}
	if watcher.ownedBySession(switchCandidate{path: "/elsewhere.jsonl", sessionID: "picked"}) {
		t.Fatal("an unrelated transcript was treated as the session's own")
	}
}
