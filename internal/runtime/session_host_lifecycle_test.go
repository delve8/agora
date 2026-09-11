package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
)

// A Host owns the Agent's lifetime, so a terminal signal aimed at the Daemon
// must not end it. Daemon.Close never signals the Host, and the Host must also
// survive SIGINT/SIGHUP that the Daemon's terminal delivers to its foreground
// process group. Only an explicit stop or the Agent's own exit ends the Host.
func TestSpawnedHostSurvivesTerminalSignals(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	agent := filepath.Join(home, "fake-pi")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nwhile IFS= read -r line; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Every RPC gets a deadline: a Host that never answers must fail the test
	// instead of blocking it until the go test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registry := NewSessionHostRegistryWithHome(os.Args[0], home)
	value := session.Session{
		ID: "pending/session-signal-test", CoordinationID: "coord-1", DaemonID: "daemon-1",
		Agent: "pi", AgentSessionID: "pi://native-signal", Workspace: workspace,
		DisplayName: "New session", State: session.StateStarting, Source: session.SourceManaged,
	}
	client, err := registry.Spawn(ctx, value, []string{agent}, nil)
	if err != nil {
		t.Fatalf("spawn host: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatalf("host state: %v", err)
	}
	// The Host must live in its own session, otherwise the Daemon's terminal
	// signals reach it through the shared foreground process group.
	if pgid, err := unix.Getpgid(metadata.HostPID); err != nil {
		t.Fatalf("host process group: %v", err)
	} else if pgid == unix.Getpgrp() {
		t.Fatalf("host %d shares the daemon process group %d", metadata.HostPID, pgid)
	}
	ownSession, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("daemon session: %v", err)
	}
	if sid, err := unix.Getsid(metadata.HostPID); err != nil {
		t.Fatalf("host session: %v", err)
	} else if sid == ownSession {
		t.Fatalf("host %d shares the daemon session %d", metadata.HostPID, sid)
	}

	for _, sig := range []unix.Signal{unix.SIGINT, unix.SIGHUP} {
		if err := unix.Kill(metadata.HostPID, sig); err != nil {
			t.Fatalf("signal %v: %v", sig, err)
		}
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := client.State(ctx); err != nil {
		t.Fatalf("host exited after a terminal signal: %v", err)
	}
	if !adapter.ProcessAlive(metadata.AgentPID) {
		t.Fatalf("agent %d exited after a terminal signal", metadata.AgentPID)
	}

	// An explicit stop still ends the Host and its Agent.
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop host: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := client.State(ctx); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not exit after an explicit stop")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if adapter.ProcessAlive(metadata.AgentPID) {
		t.Fatalf("agent %d is still running after the host exited", metadata.AgentPID)
	}
}
