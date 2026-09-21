package server

import (
	"testing"
)

// The wrapper's terminal identity has to survive the hop from the HTTP request
// to the Daemon frame: the Daemon has no terminal of its own, so this map is the
// only reason a managed Agent knows it is on a truecolor terminal.
func TestWrapperCreatePayloadCarriesTheTerminal(t *testing.T) {
	input := wrapperRequestInput{
		Agent:    "pi",
		Role:     "terminal",
		Terminal: map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
	}
	payload := wrapperCreatePayload(input, "daemon-1")
	if payload.Terminal["TERM"] != "xterm-256color" || payload.Terminal["COLORTERM"] != "truecolor" {
		t.Fatalf("payload terminal = %v, want the requesting terminal", payload.Terminal)
	}
	if payload.DaemonID != "daemon-1" || payload.Role != "terminal" {
		t.Fatalf("payload = %+v, want the request fields carried", payload)
	}
}
