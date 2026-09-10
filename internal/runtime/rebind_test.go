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

	if matched, _ := manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, []evidenceText{{text: "new question", at: time.Now()}}); matched {
		t.Fatal("matched before the provider appended anything")
	}

	appendPiUserRecord(t, path, "picked", "新会话里的问题", time.Now())

	// A line the user never typed must not confirm a switch.
	if matched, _ := manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, []evidenceText{{text: "some other text", at: time.Now()}}); matched {
		t.Fatal("matched a message the user did not submit")
	}
	if matched, _ := manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, []evidenceText{{text: "新会话里的问题", at: time.Now()}}); !matched {
		t.Fatal("did not match the appended user message")
	}

	// Records far from the submitted line are not proof, so a transcript that
	// already contains identical text can never confirm a switch by itself.
	all := piEventsAfter(t, path, 0)
	if hasMatchingUser(all, []evidenceText{{text: "an older question", at: time.Now()}}) {
		t.Fatal("a pre-existing transcript record confirmed a switch")
	}
	if hasMatchingUser(all, []evidenceText{{text: "新会话里的问题", at: time.Now().Add(-time.Hour)}}) {
		t.Fatal("a record outside the evidence skew confirmed a switch")
	}
	if !hasMatchingUser(all, []evidenceText{{text: "新会话里的问题", at: time.Now()}}) {
		t.Fatal("a freshly appended record was not accepted as evidence")
	}
}

// Multi-line input and bracketed paste reach the PTY as several submitted lines
// but the provider stores them as one user message.
func TestSwitchWatcherEvidenceCoversMultiLineInput(t *testing.T) {
	watcher := newSwitchWatcher("id", "pi", "/tmp/workspace", "own-native", "")
	watcher.recordLine("first line", time.Now())
	watcher.recordLine("second line", time.Now())
	texts := evidenceSet(watcher.evidenceTexts(time.Now()))
	for _, want := range []string{"first line second line", "second line"} {
		if _, ok := texts[want]; !ok {
			t.Fatalf("evidence is missing %q: %v", want, texts)
		}
	}
	if _, ok := texts["first line"]; ok {
		t.Fatalf("a prefix of older lines must not be evidence: %v", texts)
	}

	watcher.recordLine("too old", time.Now().Add(-2*switchLineWindow))
	if _, ok := evidenceSet(watcher.evidenceTexts(time.Now()))["too old"]; ok {
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

// Providers append a JSONL record in pieces, so a scan routinely sees a
// half-written line. The watcher must leave its cursor in front of that record
// and re-read it once it is complete; advancing to the file size would drop the
// message that confirms the switch.
func TestSwitchIncrementWaitsForCompleteRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session","id":"picked"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := info.Size()

	full := `{"type":"message","sessionId":"picked","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","message":{"role":"user","content":"half written message"}}` + "\n"
	half := len(full) / 2
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(full[:half]); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()

	manager := &Manager{}
	candidate := switchCandidate{path: path, sessionID: "picked", size: baseline}
	texts := []evidenceText{{text: "half written message", at: time.Now()}}

	matched, consumed := manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, texts)
	if matched {
		t.Fatal("a half-written record confirmed a switch")
	}
	if consumed != baseline {
		t.Fatalf("cursor advanced to %d, want it to stay at %d until the record is complete", consumed, baseline)
	}

	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(full[half:]); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()

	matched, _ = manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, texts)
	if !matched {
		t.Fatal("the completed record was not matched")
	}
	// The cursor deliberately stays at the baseline: the record is still inside
	// the evidence window, so it remains re-readable. Advancing past recent
	// records is covered by TestSwitchIncrementRetainsRecordsInsideEvidenceWindow.
}

func evidenceSet(values []evidenceText) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value.text] = struct{}{}
	}
	return result
}

// A terminal streams input-method preedit text and replacement characters, and
// editing arrives as backspaces, so the observed line is rarely identical to the
// message the provider stored. Shared phrases, not bytes, decide the match.
func TestSameUserMessageToleratesInputMethodNoise(t *testing.T) {
	stored := "这次测试的是, daemon 重启,那 pi 进程能与 daemon 自动重新连上吗?"
	observed := "这次测试的是, daemon 中\ufffd重启,那 pi \ufffd\ufffd进程能与 daemon 自动重新连上吗?"
	if !sameUserMessage(observed, stored) {
		t.Fatal("input-method noise hid a matching message")
	}
	// A backspace that removed one byte of a multi-byte character is another
	// common mangling.
	if !sameUserMessage("这次测试的是, daemon 重\xe5\x90\xaf,那 pi 进程", stored) {
		t.Fatal("a mangled line was not matched by shared phrases")
	}
	if !sameUserMessage("继续", "继续") {
		t.Fatal("a short identical message did not match")
	}
	if sameUserMessage("这次测试的是, daemon 重启", "完全不同的另一条消息,讲的是别的事情,请帮我看看构建失败的日志") {
		t.Fatal("unrelated messages matched")
	}
}

// A record written inside the evidence window must stay eligible: a scan can
// see it before the confirming line is observed, and consuming it would lose
// the switch.
func TestSwitchIncrementRetainsRecordsInsideEvidenceWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session","id":"picked"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := info.Size()
	appendPiUserRecord(t, path, "picked", "a message that does not match yet", time.Now())

	manager := &Manager{}
	candidate := switchCandidate{path: path, sessionID: "picked", size: baseline}
	matched, consumed := manager.readSwitchIncrement(context.Background(), "pi", candidate, baseline, nil)
	if matched {
		t.Fatal("matched with no evidence")
	}
	if consumed != baseline {
		t.Fatalf("cursor advanced to %d past a recent unmatched record (baseline %d)", consumed, baseline)
	}

	// A record older than the evidence window is no longer needed.
	oldPath := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(oldPath, []byte(`{"type":"session","id":"old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	appendPiUserRecord(t, oldPath, "old", "an old message", time.Now().Add(-2*switchLineWindow))
	oldCandidate := switchCandidate{path: oldPath, sessionID: "old", size: oldInfo.Size()}
	if _, consumed := manager.readSwitchIncrement(context.Background(), "pi", oldCandidate, oldInfo.Size(), nil); consumed <= oldInfo.Size() {
		t.Fatalf("cursor stayed at %d for a record outside the window", consumed)
	}
}
