package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
