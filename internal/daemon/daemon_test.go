package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
)

func TestResolveWorkspacePrefersRuntimeThenHistory(t *testing.T) {
	runtimeStore := runtime.NewMemoryStore()
	_ = runtimeStore.CreateSession(context.Background(), session.Session{ID: "daemon/daemon-1/claude://claude-1", Workspace: "/from/runtime"})
	manager := runtime.NewManager(runtimeStore, nil, nil)
	d := &Daemon{manager: manager, historySessions: []session.Session{{ID: "daemon/daemon-1/claude://claude-1", Workspace: "/from/history"}, {ID: "daemon/daemon-1/claude://claude-2", Workspace: "/second"}}}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://claude-1"); got != "/from/runtime" {
		t.Fatalf("runtime workspace = %q", got)
	}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://claude-2"); got != "/second" {
		t.Fatalf("history workspace = %q", got)
	}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://missing"); got != "" {
		t.Fatalf("missing workspace = %q", got)
	}
}

func TestReconnectDelayIsBounded(t *testing.T) {
	if got := reconnectDelay(0); got != time.Second {
		t.Fatalf("attempt 0 delay = %s", got)
	}
	if got := reconnectDelay(3); got != 8*time.Second {
		t.Fatalf("attempt 3 delay = %s", got)
	}
	if got := reconnectDelay(100); got != 32*time.Second {
		t.Fatalf("attempt 100 delay = %s", got)
	}
}

func TestWaitReconnectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitReconnect(ctx, time.Minute); err != context.Canceled {
		t.Fatalf("waitReconnect error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took too long: %s", elapsed)
	}
}
