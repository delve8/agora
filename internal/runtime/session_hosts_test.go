package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/sessionhost"
)

// newIdleHostClient builds a Client without starting a Host. Only registry
// bookkeeping is exercised, so the metadata never has to point at a live
// control socket.
func newIdleHostClient(t *testing.T, dir, hostID string) *sessionhost.Client {
	t.Helper()
	tokenPath := filepath.Join(dir, hostID+".token")
	if err := os.WriteFile(tokenPath, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, hostID+".json")
	body, err := json.Marshal(sessionhost.Metadata{
		SchemaVersion: sessionhost.MetadataVersion,
		HostID:        hostID,
		SessionID:     "daemon/d/" + hostID,
		Agent:         "pi",
		Workspace:     dir,
		ControlSocket: filepath.Join(dir, hostID+".sock"),
		TokenFile:     tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := sessionhost.NewClient(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A runtime /resume rebind rekeys the registry while the Agent stays alive.
// Input lines and the health watchdog resolve the canonical id through the
// registry, so both must follow the new key instead of the id captured when
// the subscription started.
func TestHostSessionIDFollowsRebindRekey(t *testing.T) {
	dir := t.TempDir()
	client := newIdleHostClient(t, dir, "host-1")
	registry := NewSessionHostRegistryWithHome(dir, dir)
	manager := &Manager{hosts: registry}

	registry.Put("daemon/d/pi://old", client)
	if got := manager.hostSessionID(client); got != "daemon/d/pi://old" {
		t.Fatalf("hostSessionID = %q, want the original key", got)
	}

	// Mirrors rebindSession: the same Host moves to the new canonical id.
	registry.Delete("daemon/d/pi://old")
	registry.Put("daemon/d/pi://new", client)
	if got := manager.hostSessionID(client); got != "daemon/d/pi://new" {
		t.Fatalf("hostSessionID = %q, want the rebound key", got)
	}

	registry.Delete("daemon/d/pi://new")
	if got := manager.hostSessionID(client); got != "" {
		t.Fatalf("hostSessionID = %q for an unregistered Host, want empty", got)
	}
}

// Aborted spawns, finished Hosts and killed Hosts all leave runtime state
// behind. Cleanup must remove exactly those, and never a live Host.
func TestAdoptPrunesStaleRuntimeState(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agora", "runtime", "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	socketDir := sessionhost.SocketDir()
	// Unique names: the socket directory is shared with any other running Host.
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	abortedName := "host-aborted-" + unique
	deadName := "host-dead-" + unique
	finishedName := "host-finished-" + unique
	liveName := "host-live-" + unique

	writeHost := func(hostID string, pid int, state string) {
		dir := filepath.Join(root, hostID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		tokenPath := filepath.Join(dir, "token")
		if err := os.WriteFile(tokenPath, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		controlSocket := filepath.Join(socketDir, hostID+"-c.sock")
		body, err := json.Marshal(sessionhost.Metadata{
			SchemaVersion: sessionhost.MetadataVersion, HostID: hostID,
			SessionID: "daemon/d/pi://" + hostID, Agent: "pi", Workspace: root,
			HostPID: pid, ControlSocket: controlSocket, TokenFile: tokenPath, State: state,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		// A Host killed with SIGKILL cannot remove its sockets.
		if err := os.WriteFile(controlSocket, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(socketDir, hostID+"-a.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	aborted := filepath.Join(root, abortedName)
	if err := os.MkdirAll(aborted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aborted, "launch.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A definitely dead PID: the maximum pid value is not in use.
	writeHost(deadName, 0x7FFFFFFF, "running")
	writeHost(finishedName, os.Getpid(), "exited")
	writeHost(liveName, os.Getpid(), "running")

	registry := NewSessionHostRegistryWithHome("agora", home)
	if _, err := registry.Adopt(context.Background()); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	for _, gone := range []string{abortedName, deadName, finishedName} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("stale runtime dir %s was not pruned: %v", gone, err)
		}
		if _, err := os.Stat(filepath.Join(socketDir, gone+"-c.sock")); !os.IsNotExist(err) {
			t.Fatalf("stale socket for %s was not removed: %v", gone, err)
		}
	}
	// A Host whose process is still alive is kept: an unresponsive Host may still
	// own a running Agent, and its metadata is the only way back to it.
	if _, err := os.Stat(filepath.Join(root, liveName, "metadata.json")); err != nil {
		t.Fatalf("live Host runtime state was removed: %v", err)
	}
}
