package sessionhost

import (
	"strings"
	"testing"
)

// piStartup is the exact terminal setup Pi writes before its TUI paints: enable
// bracketed paste, push kitty keyboard-protocol flags, ask which flags are in
// effect, and query device attributes. Captured from pi 0.85.1.
const piStartup = "\x1b[?2004h\x1b[>7u\x1b[?u\x1b[c\x1b[?25l"

// The Agent starts before any client can attach, so nobody answers its keyboard
// query. Unanswered, Pi enables neither the kitty protocol nor modifyOtherKeys
// and Shift+Enter stays indistinguishable from Enter, so the Host answers as the
// terminal would — with the flags the Agent itself asked for.
func TestTerminalObserverAnswersKeyboardQueryWithoutAClient(t *testing.T) {
	observer := newTerminalObserver()
	reply := observer.Observe([]byte(piStartup), false)
	if !strings.Contains(string(reply), "\x1b[?7u") {
		t.Fatalf("reply = %q, want the kitty flags the Agent asked for", reply)
	}
	// The answer is a report, not a mode: the Agent asked for flags 7 itself.
	if observer.KittyFlags() != 7 {
		t.Fatalf("tracked kitty flags = %d, want 7", observer.KittyFlags())
	}
}

// A real terminal is the better authority whenever it is actually listening, so
// the Host does not answer a query that a client receives.
func TestTerminalObserverLeavesTheQueryToAnAttachedClient(t *testing.T) {
	observer := newTerminalObserver()
	if reply := observer.Observe([]byte(piStartup), true); len(reply) != 0 {
		t.Fatalf("reply = %q, want none while a client is attached", reply)
	}
}

// The query arrives in whatever chunks the PTY read returns; a split sequence
// must still be answered.
func TestTerminalObserverHandlesSplitSequences(t *testing.T) {
	observer := newTerminalObserver()
	var reply []byte
	for _, b := range []byte(piStartup) {
		reply = append(reply, observer.Observe([]byte{b}, false)...)
	}
	if !strings.Contains(string(reply), "\x1b[?7u") {
		t.Fatalf("reply = %q, want the answer to a byte-by-byte query", reply)
	}
}

// An Agent that never asks for the kitty protocol gets the "no flags" answer,
// which is what makes Pi fall back to xterm's modifyOtherKeys.
func TestTerminalObserverAnswersNoFlagsWithoutAPush(t *testing.T) {
	observer := newTerminalObserver()
	reply := observer.Observe([]byte("\x1b[?u"), false)
	if string(reply) != "\x1b[?0u" {
		t.Fatalf("reply = %q, want the no-flags answer", reply)
	}
}

// Everything the Agent enabled while nobody was watching has to be replayed to
// the next client, otherwise a pasted newline arrives as a submit and Shift+Enter
// arrives as Enter.
func TestTerminalObserverReplaysEnabledModes(t *testing.T) {
	observer := newTerminalObserver()
	observer.Observe([]byte(piStartup), false)
	observer.Observe([]byte("\x1b[?1h\x1b[?1000h\x1b[?1002h\x1b[?1004h\x1b[?1006h"), false)
	observer.Observe([]byte("\x1b[>4;2m"), false)

	replay := observer.Replay()
	for _, want := range []string{"\x1b[?2004h", "\x1b[?1h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h", "\x1b[=7;1u", "\x1b[>4;2m"} {
		if !strings.Contains(replay, want) {
			t.Errorf("replay %q is missing %q", replay, want)
		}
	}
	// Bracketed paste changes what the user's input means, so it must be on
	// before the client can type or paste anything.
	if !strings.HasPrefix(replay, "\x1b[?2004h") {
		t.Fatalf("replay = %q, want bracketed paste first", replay)
	}
}

// Modes the Agent switched off must not be replayed, or an attaching client
// would inherit a terminal state the Agent no longer expects.
func TestTerminalObserverDropsDisabledModes(t *testing.T) {
	observer := newTerminalObserver()
	observer.Observe([]byte(piStartup), false)
	observer.Observe([]byte("\x1b[?1h\x1b[?1000h\x1b[?1006h\x1b[>4;2m"), false)
	// Pi disables its modes on exit: bracketed paste, kitty flags, modifiers,
	// and mouse reporting as one set.
	observer.Observe([]byte("\x1b[?2004l\x1b[<u\x1b[>4;0m\x1b[?1006l\x1b[?1004l\x1b[?1003l\x1b[?1002l\x1b[?1000l\x1b[?1l"), false)

	if replay := observer.Replay(); replay != "" {
		t.Fatalf("replay = %q, want nothing once every mode was disabled", replay)
	}
	if observer.KittyFlags() != 0 {
		t.Fatalf("tracked kitty flags = %d, want 0 after a pop", observer.KittyFlags())
	}
}

// Ordinary output must not confuse the scanner.
func TestTerminalObserverIgnoresOrdinaryOutput(t *testing.T) {
	observer := newTerminalObserver()
	observer.Observe([]byte("hello \x1b[31mred\x1b[0m world\r\n"), false)
	observer.Observe([]byte("\x1b]0;a title\x07"), false)
	observer.Observe([]byte("\x1b"), false)
	observer.Observe([]byte("[32mgreen"), false)
	if replay := observer.Replay(); replay != "" {
		t.Fatalf("replay = %q, want no modes from coloured text", replay)
	}
}

// The kitty "set flags" form is tracked like a push, so an Agent that uses it
// instead of the push/pop pair is replayed too.
func TestTerminalObserverTracksKittySetFlags(t *testing.T) {
	observer := newTerminalObserver()
	observer.Observe([]byte("\x1b[=3;1u"), false)
	if replay := observer.Replay(); !strings.Contains(replay, "\x1b[=3;1u") {
		t.Fatalf("replay = %q, want the flags the Agent set", replay)
	}
	observer.Observe([]byte("\x1b[=3;3u"), false)
	if replay := observer.Replay(); strings.Contains(replay, "u") {
		t.Fatalf("replay = %q, want nothing after the flags were reset", replay)
	}
}
