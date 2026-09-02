package sessionhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostLifecycleAndAuthenticatedControl(t *testing.T) {
	runtimeDir := t.TempDir()
	host, err := New(Config{
		HostID:     "host-test",
		SessionID:  "daemon/test/pi://native-test",
		DaemonID:   "test",
		Agent:      "pi",
		Workspace:  t.TempDir(),
		Command:    []string{"/bin/sh", "-c", "sleep 30"},
		RuntimeDir: runtimeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()

	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	var client *Client
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(metadataPath); statErr == nil {
			client, err = NewClient(metadataPath)
			if err == nil {
				if _, err = client.State(context.Background()); err == nil {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil || err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("host did not become controllable: client=%v err=%v run=%v", client != nil, err, runErr)
		default:
			t.Fatalf("host did not become controllable: client=%v err=%v", client != nil, err)
		}
	}
	if got := client.Metadata().Agent; got != "pi" {
		t.Fatalf("agent = %q, want pi", got)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session-host did not exit after stopping its Agent")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory still exists after cleanup: %v", err)
	}
}

func TestHostRejectsInvalidToken(t *testing.T) {
	runtimeDir := t.TempDir()
	host, err := New(Config{SessionID: "session-1", Agent: "pi", Workspace: t.TempDir(), Command: []string{"/bin/sh", "-c", "sleep 30"}, RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()
	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(metadataPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	metadata, err := LoadMetadata(metadataPath)
	if err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("load metadata: %v (host exited: %v)", err, runErr)
		default:
			t.Fatal(err)
		}
	}
	conn, err := (&netDialer{}).dial(context.Background(), metadata.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request{Version: MetadataVersion, Token: "wrong", Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var reply response
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK || reply.Error != "invalid host token" {
		t.Fatalf("unexpected invalid-token response: %+v", reply)
	}
	client, err := NewClient(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Stop(context.Background())
	<-done
}
