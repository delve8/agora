package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/session"
)

// runAttachCommand is the terminal-facing entry point for reaching a managed
// session.
//
//   - `agora attach` prints what exists in the working directory together with
//     how to use it, because a canonical session id is not something a person
//     remembers.
//   - `agora attach list [--all]` lists the sessions of this machine.
//   - `agora attach <# | session-id | provider-id | unique prefix>` attaches.
func runAttachCommand(args []string) error {
	if len(args) == 0 {
		return printSessionList(true)
	}
	if args[0] == "list" || args[0] == "ls" {
		all := false
		for _, arg := range args[1:] {
			switch arg {
			case "-a", "--all":
				all = true
			default:
				return fmt.Errorf("unknown option %q for `agora attach list` (only --all is supported)", arg)
			}
		}
		return printSessionList(all)
	}
	if len(args) > 1 {
		return errors.New("`agora attach` takes one session reference: run `agora attach list` to see the choices")
	}
	reference := strings.TrimSpace(args[0])
	entry, err := resolveSessionArgument(reference)
	if err != nil {
		// A Daemon that predates session listing cannot answer the question, but
		// it can still attach to an id it is given, which is the old behaviour.
		if _, ok := err.(sessionListingUnsupportedError); ok {
			if _, parseErr := session.ParseSessionID(reference); parseErr == nil {
				return runAttach(reference)
			}
		}
		return err
	}
	if !entry.Attachable {
		return fmt.Errorf("session %s is not running, so there is no terminal to attach to\n"+
			"History sessions are continued from inside the Agent: run `agora wrap %s` and pick it with /resume",
			shortSessionReference(entry), entry.Agent)
	}
	return runAttach(entry.SessionID)
}

// printSessionList shows the sessions of the working directory, or of the whole
// machine with all=true, followed by the hints that make the numbering usable.
func printSessionList(all bool) error {
	entries, err := sessionListings(workspaceOrEmpty(all))
	if err != nil {
		return err
	}
	sortSessionListings(entries)
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "No Agora sessions "+scopeDescription(all)+".")
		fmt.Fprintln(os.Stderr, "Start one with `agora wrap pi` (or `agora wrap claude`), or list every session with `agora attach list --all`.")
		return nil
	}
	fmt.Print(formatSessionList(entries, time.Now(), all))
	fmt.Fprintln(os.Stderr)
	if countAttachable(entries) > 0 {
		fmt.Fprintln(os.Stderr, "Attach to a running session with `agora attach <#>` or `agora attach <session-id>`.")
	}
	fmt.Fprintln(os.Stderr, "History sessions are continued from inside the Agent: run `agora wrap <agent>` and use /resume.")
	return nil
}

func workspaceOrEmpty(all bool) string {
	if all {
		return ""
	}
	workspace, err := os.Getwd()
	if err != nil {
		return ""
	}
	return workspace
}

func scopeDescription(all bool) string {
	if all {
		return "on this machine"
	}
	return "in this directory"
}

// sessionListings asks the local Daemon, which is the only party that knows both
// the sessions it manages and the provider history on this machine. A workspace
// narrows the answer; empty asks for every session.
func sessionListings(workspace string) ([]protocol.SessionListEntry, error) {
	conn, err := dialDaemon()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	request := protocol.SessionListRequest{Type: protocol.SessionList, Agent: unsupportedListAgent}
	if workspace != "" {
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return nil, err
		}
		request.Workspace = absolute
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("send session list request to daemon: %w", err)
	}
	var response protocol.SessionListResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("read session list from daemon: %w", err)
	}
	if response.Error != "" {
		return nil, describeListError(response.Error)
	}
	return response.Sessions, nil
}

// unsupportedListAgent is sent with every list request so an older Daemon cannot
// mistake it for a wrapper request and create a session.
const unsupportedListAgent = "agora-session-list"

// sessionListingUnsupportedError means the Daemon predates session listing, so a
// caller that already holds a canonical id can still proceed.
type sessionListingUnsupportedError struct{}

func (sessionListingUnsupportedError) Error() string {
	return "the running Agora daemon does not support listing sessions: restart it (`agora daemon`) and try again"
}

// describeListError turns the reply of a Daemon that predates session listing
// into instructions rather than into whatever the Server complained about.
func describeListError(message string) error {
	for _, hint := range []string{"unsupported agent", "workspace is required"} {
		if strings.Contains(message, hint) {
			return sessionListingUnsupportedError{}
		}
	}
	return errors.New(message)
}

func dialDaemon() (net.Conn, error) {
	path, err := daemonSocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, daemonUnavailableError{path: path, err: err}
	}
	return conn, nil
}

// sortSessionListings puts what can be attached first: the list exists to pick a
// session to attach to, and a history session cannot be attached to.
func sortSessionListings(entries []protocol.SessionListEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Attachable != entries[j].Attachable {
			return entries[i].Attachable
		}
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
}

func countAttachable(entries []protocol.SessionListEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Attachable {
			count++
		}
	}
	return count
}

func formatSessionList(entries []protocol.SessionListEntry, now time.Time, showWorkspace bool) string {
	headers := []string{"#", "NAME", "AGENT", "STATE", "UPDATED"}
	if showWorkspace {
		headers = append(headers, "WORKSPACE")
	}
	headers = append(headers, "SESSION")
	rows := make([][]string, 0, len(entries))
	for index, entry := range entries {
		name := strings.TrimSpace(entry.DisplayName)
		if name == "" {
			name = "(unnamed)"
		}
		state := entry.State
		if !entry.Attachable {
			state = "history"
		}
		row := []string{
			strconv.Itoa(index + 1),
			name,
			entry.Agent,
			state,
			humanAge(now, entry.UpdatedAt),
		}
		if showWorkspace {
			row = append(row, entry.Workspace)
		}
		row = append(row, shortSessionReference(entry))
		rows = append(rows, row)
	}
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = len(header)
	}
	for _, row := range rows {
		for index, cell := range row {
			if len(cell) > widths[index] {
				widths[index] = len(cell)
			}
		}
	}
	var out strings.Builder
	writeRow := func(row []string) {
		for index, cell := range row {
			if index > 0 {
				out.WriteString("  ")
			}
			out.WriteString(cell)
			if index < len(row)-1 {
				out.WriteString(strings.Repeat(" ", widths[index]-len(cell)))
			}
		}
		out.WriteString("\n")
	}
	writeRow(headers)
	for _, row := range rows {
		writeRow(row)
	}
	return out.String()
}

// shortSessionReference prefers the provider's own id: it is shorter and more
// recognisable, and prefix matching makes the truncated form valid input.
func shortSessionReference(entry protocol.SessionListEntry) string {
	reference := entry.AgentSessionID
	if reference == "" {
		reference = entry.SessionID
	}
	const limit = 40
	if len(reference) > limit {
		return reference[:limit]
	}
	return reference
}

func humanAge(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	delta := now.Sub(then)
	if delta < 0 {
		delta = 0
	}
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(delta.Hours()/24))
	}
}

// resolveSessionArgument turns what the user typed into a listed session: the
// number from `agora attach` in this directory, or an id, provider id, or unique
// prefix of either. An explicit id is also searched across workspaces, since only
// the bare number is scoped to the current directory.
func resolveSessionArgument(argument string) (protocol.SessionListEntry, error) {
	if argument == "" {
		return protocol.SessionListEntry{}, errors.New("no session given: run `agora attach list` to see the choices")
	}
	local, err := sessionListings(workspaceOrEmpty(false))
	if err != nil {
		return protocol.SessionListEntry{}, err
	}
	sortSessionListings(local)
	if index, err := strconv.Atoi(argument); err == nil {
		if index < 1 || index > len(local) {
			return protocol.SessionListEntry{}, fmt.Errorf("session %d does not exist: `agora attach` lists %d session(s) in this directory (`agora attach list --all` shows every session)", index, len(local))
		}
		return local[index-1], nil
	}
	if entry, matches := matchSession(local, argument); matches > 1 {
		return protocol.SessionListEntry{}, ambiguousSessionError(argument, local)
	} else if matches == 1 {
		return entry, nil
	}
	everywhere, err := sessionListings("")
	if err != nil {
		return protocol.SessionListEntry{}, err
	}
	if entry, matches := matchSession(everywhere, argument); matches > 1 {
		return protocol.SessionListEntry{}, ambiguousSessionError(argument, everywhere)
	} else if matches == 1 {
		return entry, nil
	}
	return protocol.SessionListEntry{}, fmt.Errorf("no session matches %q: run `agora attach list` to see the choices", argument)
}

func matchSession(entries []protocol.SessionListEntry, argument string) (protocol.SessionListEntry, int) {
	var matches []protocol.SessionListEntry
	for _, entry := range entries {
		switch {
		case argument == entry.SessionID || (entry.AgentSessionID != "" && argument == entry.AgentSessionID):
			return entry, 1
		case strings.HasPrefix(entry.SessionID, argument) || (entry.AgentSessionID != "" && strings.HasPrefix(entry.AgentSessionID, argument)):
			matches = append(matches, entry)
		}
	}
	if len(matches) == 1 {
		return matches[0], 1
	}
	return protocol.SessionListEntry{}, len(matches)
}

func ambiguousSessionError(argument string, entries []protocol.SessionListEntry) error {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.SessionID, argument) || (entry.AgentSessionID != "" && strings.HasPrefix(entry.AgentSessionID, argument)) {
			names = append(names, shortSessionReference(entry))
		}
	}
	return fmt.Errorf("%q matches %d sessions (%s): use more of the id", argument, len(names), strings.Join(names, ", "))
}
