package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func handoffFixture() []HandoffMessage {
	return []HandoffMessage{
		{Role: "user", Content: "这是从另一个 Agent 交接过来的资料：已完成 X，待办 Y。请视为既定事实，无需重新推导。"},
		{Role: "assistant", Content: "已了解交接内容：已完成 X，待办 Y。"},
	}
}

// Pi discovers a session by reading the first line's id and parsing the
// id/parentId chain, so the synthesized file must survive FindPiHistoryBySessionID
// and scanPiHistorySummary.
func TestPiSessionWriterProducesDiscoverableSession(t *testing.T) {
	sessionDir := t.TempDir()
	workspace := t.TempDir()
	writer := PiSessionWriter{SessionDir: sessionDir, Provider: "anthropic", Model: "claude-sonnet-4"}
	nativeID, err := NewNativeSessionID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := writer.WriteSessionFile(workspace, nativeID, handoffFixture())
	if err != nil {
		t.Fatal(err)
	}
	if found := FindPiHistoryBySessionID(sessionDir, nativeID); found != path {
		t.Fatalf("FindPiHistoryBySessionID = %q, want %q", found, path)
	}
	summary, err := scanPiHistorySummary(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SessionID != nativeID {
		t.Fatalf("summary session id = %q, want %q", summary.SessionID, nativeID)
	}
	if !strings.Contains(summary.FirstUser, "交接") {
		t.Fatalf("first user message was not parsed: %q", summary.FirstUser)
	}
	if summary.Workspace == "" {
		t.Fatal("workspace was not recorded in the Pi session")
	}
	// A second write for the same id must not silently overwrite a session.
	if _, err := writer.WriteSessionFile(workspace, nativeID, handoffFixture()); err == nil {
		t.Fatal("expected a duplicate native id to be rejected")
	}
}

// Claude resolves --resume by scanning the project directory derived from the
// canonical workspace path. The file must land there and expose the id.
func TestClaudeSessionWriterProducesDiscoverableSession(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writer := ClaudeSessionWriter{HomeDir: home, Version: "2.1.278"}
	nativeID, err := NewNativeSessionID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := writer.WriteSessionFile(workspace, nativeID, handoffFixture())
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := CanonicalWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	expectedDir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", "_", "-").Replace(cwd))
	if filepath.Dir(path) != expectedDir {
		t.Fatalf("session written to %q, want %q", filepath.Dir(path), expectedDir)
	}
	if found := FindHistoryBySessionID(home, nativeID); found != path {
		t.Fatalf("FindHistoryBySessionID = %q, want %q", found, path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var user, assistant bool
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		record, err := ParseHistoryEvent(nativeID, []byte(line))
		if err != nil {
			t.Fatalf("parse record: %v", err)
		}
		switch record.Event.Kind {
		case "user":
			user = true
			if !strings.Contains(record.Event.Content, "交接") {
				t.Fatalf("user content = %q", record.Event.Content)
			}
		case "assistant":
			assistant = true
		}
	}
	if !user || !assistant {
		t.Fatalf("parsed user=%v assistant=%v, want both", user, assistant)
	}
}

// Real-binary acceptance is the only check that proves the synthesized file is
// what the provider actually reads. These run only when the caller points at an
// installed binary, so CI without Agents stays green.
func TestPiSessionWriterAcceptedByRealPi(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("AGORA_TEST_PI_BINARY"))
	if binary == "" {
		t.Skip("AGORA_TEST_PI_BINARY not set")
	}
	sessionDir := t.TempDir()
	workspace := t.TempDir()
	nativeID, err := NewNativeSessionID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := (PiSessionWriter{SessionDir: sessionDir, Provider: "anthropic", Model: "claude-sonnet-4"}).WriteSessionFile(workspace, nativeID, handoffFixture())
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	cmd := exec.Command(binary, "--export", path)
	cmd.Dir = outDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pi --export failed: %v\n%s", err, output)
	}
	matches, _ := filepath.Glob(filepath.Join(outDir, "*.html"))
	if len(matches) == 0 {
		t.Fatalf("pi produced no export for the synthesized session: %s", output)
	}
}

func TestClaudeSessionWriterAcceptedByRealClaude(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("AGORA_TEST_CLAUDE_BINARY"))
	if binary == "" {
		t.Skip("AGORA_TEST_CLAUDE_BINARY not set")
	}
	workspace := t.TempDir()
	nativeID, err := NewNativeSessionID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := (ClaudeSessionWriter{Version: os.Getenv("AGORA_TEST_CLAUDE_VERSION")}).WriteSessionFile(workspace, nativeID, handoffFixture())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--bare", "--resume", nativeID, "-p", "continue")
	cmd.Dir = workspace
	cmd.Stdin = strings.NewReader("")
	output, _ := cmd.CombinedOutput()
	if strings.Contains(string(output), "No conversation found") {
		t.Fatalf("claude did not find the synthesized session: %s", output)
	}
}

func TestSessionWriterFactory(t *testing.T) {
	if writer, ok := NewSessionFileWriter("pi", "/home", "/sessions", ""); !ok || writer.Agent() != "pi" {
		t.Fatalf("pi writer = %#v, %v", writer, ok)
	}
	if writer, ok := NewSessionFileWriter("claude-code", "/home", "", "2.1.278"); !ok || writer.Agent() != "claude" {
		t.Fatalf("claude writer = %#v, %v", writer, ok)
	}
	if _, ok := NewSessionFileWriter("opencode", "/home", "", ""); ok {
		t.Fatal("opencode must not claim a transcript writer")
	}
}

func TestSessionWriterRejectsUnsupportedMessages(t *testing.T) {
	writer := PiSessionWriter{SessionDir: t.TempDir()}
	if _, err := writer.WriteSessionFile(t.TempDir(), "11111111-1111-4111-8111-111111111111", nil); err == nil {
		t.Fatal("expected an empty handoff to be rejected")
	}
	if _, err := writer.WriteSessionFile(t.TempDir(), "11111111-1111-4111-8111-111111111111", []HandoffMessage{{Role: "system", Content: "x"}}); err == nil {
		t.Fatal("expected a system role to be rejected")
	}
}
