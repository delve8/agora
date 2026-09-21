package main

import (
	"errors"
	"testing"
)

// Pi's own CLI commands and one-shot switches are not Agent sessions. The
// wrapper must let the real binary run them instead of asking the Daemon for a
// session around a command that exits on its own (`pi update`).
func TestPiWrapperPassesThroughCLICommands(t *testing.T) {
	cases := [][]string{
		{"update"},
		{"update", "--all"},
		{"install", "git:host/user/repo"},
		{"remove", "git:host/user/repo"},
		{"uninstall", "git:host/user/repo"},
		{"list"},
		{"config"},
		{"auth", "status"},
		{"--help"},
		{"-h"},
		{"--version"},
		{"-v"},
		{"-p", "summarise the diff"},
		{"--print", "summarise the diff"},
		{"--export", "session.html"},
		{"--list-models"},
		{"--mode", "json"},
		{"--mode=rpc"},
	}
	for _, args := range cases {
		err := runPiWrapper(args)
		var passthrough passthroughAgentError
		if !errors.As(err, &passthrough) {
			t.Errorf("pi %v: error = %v, want a passthrough answer for the PATH wrapper", args, err)
			continue
		}
		if passthrough.ExitCode() != 76 {
			t.Errorf("pi %v: exit code = %d, want 76", args, passthrough.ExitCode())
		}
	}
}

// Everything else is an interactive Agent session and stays managed, including
// the arguments that merely look like a command: after `--` Pi treats the rest
// as messages, and the first argument is the only one Pi reads as a command.
func TestPiWrapperKeepsAgentSessionsManaged(t *testing.T) {
	cases := [][]string{
		{},
		{"fix the failing test"},
		{"--", "update"},
		{"--offline"},
		{"--provider", "google", "explain this repository"},
		{"--session-id", "b1aa0f72-f174-4ac9-a0c5-6da7fc53bf78"},
		{"--continue"},
		{"-n", "release work"},
		{"--mode", "text", "explain this"},
	}
	for _, args := range cases {
		if !piRunsAnAgentSession(args) {
			t.Errorf("pi %v: treated as a CLI command, want a managed Agent session", args)
		}
	}
}

// The classifier mirrors Pi's reading of argv, so the same arguments produce the
// same decision on both sides.
func TestPiRunsAnAgentSessionMatchesPiCommands(t *testing.T) {
	if piRunsAnAgentSession([]string{"update"}) {
		t.Fatal("pi update was treated as an Agent session")
	}
	if !piRunsAnAgentSession([]string{"update that changelog"}) {
		t.Fatal("a prompt that starts with the word update was treated as a command")
	}
	if !piRunsAnAgentSession([]string{"--", "update"}) {
		t.Fatal("a message after -- was treated as a command")
	}
	if piRunsAnAgentSession([]string{"-p", "explain this"}) {
		t.Fatal("a non-interactive print run was treated as a managed session")
	}
	if piRunsAnAgentSession([]string{"--mode", "json"}) {
		t.Fatal("a structured event stream was treated as a managed session")
	}
	if !piRunsAnAgentSession([]string{"--mode", "text", "explain this"}) {
		t.Fatal("the default text mode should stay a managed session")
	}
}
