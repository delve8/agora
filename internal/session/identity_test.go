package session

import "testing"

func TestCanonicalSessionID(t *testing.T) {
	id, err := NewSessionID("workstation-1", "claude", "claude://12345")
	if err != nil {
		t.Fatal(err)
	}
	if id != "daemon/workstation-1/claude://12345" {
		t.Fatalf("unexpected id %q", id)
	}
	identity, err := ParseSessionID(id)
	if err != nil {
		t.Fatal(err)
	}
	if identity.DaemonID != "workstation-1" || identity.Agent != "claude" || identity.AgentSessionID != "claude://12345" {
		t.Fatalf("unexpected identity %+v", identity)
	}
}

func TestCanonicalSessionIDRejectsMismatch(t *testing.T) {
	if _, err := NewSessionID("workstation-1", "claude", "opencode://12345"); err == nil {
		t.Fatal("expected agent mismatch")
	}
	if _, err := ParseSessionID("daemon/workstation-1/claude://a/b"); err == nil {
		t.Fatal("expected invalid native session id")
	}
}
