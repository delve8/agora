package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
)

// The canonical Session ID is derived from the provider's native id, so a
// provider context switch renames the session while a caller may still hold the
// previous id. A terminal wrapper does exactly that: it attaches with the id the
// Daemon returned when it created the session, and a Pi process that picks its
// own session id (any invocation that forwards the user's arguments, such as
// `pi update`) rekeys that session moments later.
func TestRekeyedSessionIDKeepsResolving(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manager := NewManager(store, nil, nil)

	first := "daemon-1/pi://session-a"
	second := "daemon-1/pi://session-b"
	if err := store.CreateSession(ctx, sessionAt(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RekeySession(ctx, first, second); err != nil {
		t.Fatalf("rekey: %v", err)
	}

	value, err := manager.GetSession(ctx, first)
	if err != nil {
		t.Fatalf("the id the caller was handed stopped resolving: %v", err)
	}
	if value.ID != second {
		t.Fatalf("stale id resolved to %q, want %q", value.ID, second)
	}
	if got := manager.ResolveSessionID(first); got != second {
		t.Fatalf("ResolveSessionID(%q) = %q, want %q", first, got, second)
	}
	if got := manager.ResolveSessionID("daemon-1/pi://untouched"); got != "daemon-1/pi://untouched" {
		t.Fatalf("an id that was never rekeyed was redirected to %q", got)
	}
	if _, err := manager.GetSession(ctx, "daemon-1/pi://never-existed"); err == nil {
		t.Fatal("an unknown session id resolved")
	}
}

// A session can move back to an id it used before (/resume back to an older
// conversation). The redirect table has to stay acyclic, or resolution would
// spin forever.
func TestRekeyAliasesSurviveReturningToAnEarlierID(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manager := NewManager(store, nil, nil)

	first := "daemon-1/pi://session-a"
	second := "daemon-1/pi://session-b"
	if err := store.CreateSession(ctx, sessionAt(first)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, sessionAt(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RekeySession(ctx, first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RekeySession(ctx, second, first); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{first, second} {
		value, err := manager.GetSession(ctx, id)
		if err != nil {
			t.Fatalf("GetSession(%q): %v", id, err)
		}
		if value.ID != first {
			t.Fatalf("GetSession(%q) = %q, want the session now named %q", id, value.ID, first)
		}
	}
}

func TestRekeyAliasesAreBounded(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manager := NewManager(store, nil, nil)

	current := "daemon-1/pi://session-0"
	if err := store.CreateSession(ctx, sessionAt(current)); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= maxRekeyAliases+64; index++ {
		next := fmt.Sprintf("daemon-1/pi://session-%d", index)
		if err := store.CreateSession(ctx, sessionAt(next)); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.RekeySession(ctx, current, next); err != nil {
			t.Fatalf("rekey %d: %v", index, err)
		}
		current = next
	}

	manager.rekeyMu.Lock()
	size := len(manager.rekeys)
	manager.rekeyMu.Unlock()
	if size > maxRekeyAliases {
		t.Fatalf("redirect table holds %d entries, want at most %d", size, maxRekeyAliases)
	}
	if _, err := manager.GetSession(ctx, current); err != nil {
		t.Fatalf("the most recent id stopped resolving: %v", err)
	}
}

// The failure this guards against: a wrapper creates a hosted Pi session by
// forwarding the user's arguments, so the Daemon cannot pass `--session-id` and
// Pi creates its own session. The injected reporter reveals that id moments
// after creation, the session is rekeyed, and the wrapper then attaches with the
// id it was handed. That attach must reach the running Agent instead of failing
// with "session not found".
func TestAttachFollowsProviderRebindAfterCreation(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(home, "fake-pi")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nwhile IFS= read -r line; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewDaemonManager(NewMemoryStore(), "daemon-1", adapter.NewClaudeCodeAdapter(""), NewClaudeProvider("", home))
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(NewPiProvider(PiConfig{Binary: agent, SessionDir: sessionDir}))
	defer manager.Close()

	ctx := context.Background()
	provisional := "pending/session-attach-rebind"
	created, err := manager.CreateManagedSessionWithAgentArgs(ctx, provisional, "coord-1", workspace, "New session", "terminal", "pi", []string{"update"})
	if err != nil {
		t.Fatalf("create hosted session: %v", err)
	}
	assigned := strings.TrimPrefix(created.AgentSessionID, "pi://")
	if assigned == "" {
		t.Fatalf("hosted session has no assigned native id: %+v", created)
	}
	if _, err := manager.SetAgentIdentity(ctx, provisional, "pi", "pi://"+assigned); err != nil {
		t.Fatalf("set agent identity: %v", err)
	}
	canonical, err := manager.RekeySession(ctx, provisional, canonicalPiID(t, "daemon-1", assigned))
	if err != nil {
		t.Fatalf("rekey to the assigned canonical id: %v", err)
	}
	client, ok := manager.hosts.Get(canonical.ID)
	if !ok {
		t.Fatalf("host is not registered under %s", canonical.ID)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	// The provider reports the session it actually created, which is not the id
	// the Daemon assigned because `--session-id` was left out of an invocation
	// that forwards the user's arguments.
	const reported = "01a0c275-dbb1-70f0-aa23-c8e4b4b715c8"
	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := manager.ReportAgentSession(ctx, AgentSessionReport{HostID: metadata.HostID, Reason: "startup", NativeSessionID: reported})
	if err != nil {
		t.Fatalf("report the provider session: %v", err)
	}
	if rebound.ID == canonical.ID {
		t.Fatalf("report did not rekey the session: %s", rebound.ID)
	}

	// What the attach handler does: resolve the id the caller holds, then ask the
	// resolved session for its socket.
	resolved, err := manager.GetSession(ctx, canonical.ID)
	if err != nil {
		t.Fatalf("attach id %s stopped resolving after the provider rebind: %v", canonical.ID, err)
	}
	if resolved.ID != rebound.ID {
		t.Fatalf("attach id resolved to %q, want %q", resolved.ID, rebound.ID)
	}
	staleSocket, err := manager.AttachAddr(resolved.ID)
	if err != nil {
		t.Fatalf("attach through the stale id: %v", err)
	}
	currentSocket, err := manager.AttachAddr(rebound.ID)
	if err != nil {
		t.Fatalf("attach through the current id: %v", err)
	}
	if staleSocket == "" || staleSocket != currentSocket {
		t.Fatalf("attach sockets differ: stale=%q current=%q", staleSocket, currentSocket)
	}
}

func sessionAt(id string) session.Session {
	return session.Session{ID: id, Agent: "pi", AgentSessionID: "pi://" + strings.TrimPrefix(id, "daemon-1/pi://"), Workspace: "/tmp"}
}

func canonicalPiID(t *testing.T, daemonID, nativeID string) string {
	t.Helper()
	value, err := session.NewSessionID(daemonID, "pi", "pi://"+nativeID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
