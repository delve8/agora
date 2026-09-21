package sessionhost

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// terminalObserver is the Host acting as the Agent's terminal.
//
// A managed Agent starts before any client attaches, so the terminal setup it
// performs at startup is otherwise lost twice over: the mode switches it sends
// have no receiver, and the capability queries it writes have nobody to answer.
// Pi asks for the kitty keyboard protocol and then waits about 150ms for an
// answer; unanswered, it enables neither that protocol nor xterm's
// modifyOtherKeys, so Shift+Enter stays indistinguishable from Enter, and a
// client attaching later never learns that bracketed paste was enabled, which is
// what turns a pasted newline into a submit.
//
// The Host therefore answers the keyboard-protocol query itself and remembers
// the persistent modes the Agent enabled, so a client that attaches at any point
// gets them replayed. The query is only answered when no client is attached: a
// real terminal is the better authority whenever it is actually listening.
type terminalObserver struct {
	mu         sync.Mutex
	pending    []byte
	kittyFlags int
	// modes maps a mode name to the exact sequence that turns it on, so replay
	// is idempotent and never has to reason about the Agent's history.
	modes map[string]string
}

// maxPendingEscape bounds the bytes kept for an unfinished escape sequence. A
// longer run cannot be a mode sequence this observer cares about.
const maxPendingEscape = 64

// replayOrder fixes the order modes are replayed in. Bracketed paste and mouse
// reporting come first because they change what the user's input means, and
// application cursor keys last because they only affect arrow keys.
var replayOrder = []string{"paste", "mouse", "focus", "appcursor", "keyboard", "modifyOtherKeys"}

func newTerminalObserver() *terminalObserver {
	return &terminalObserver{modes: make(map[string]string)}
}

// Observe consumes Agent output and returns the bytes the Host has to write back
// to the Agent, if any. attached reports whether a client is receiving this same
// output, which decides whether the Host answers a capability query itself.
func (t *terminalObserver) Observe(data []byte, attached bool) []byte {
	if t == nil || len(data) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = append(t.pending, data...)

	var reply []byte
	index := 0
	for index < len(t.pending) {
		if t.pending[index] != 0x1b {
			index++
			continue
		}
		if index+1 >= len(t.pending) {
			break
		}
		if t.pending[index+1] != '[' {
			// Not a CSI sequence (OSC, a bare Escape, ...). Only observation is
			// affected, so skipping it is always safe.
			index += 2
			continue
		}
		final := index + 2
		for final < len(t.pending) && (t.pending[final] < 0x40 || t.pending[final] > 0x7e) {
			final++
		}
		if final >= len(t.pending) {
			break
		}
		reply = append(reply, t.apply(t.pending[index:final+1], attached)...)
		index = final + 1
	}
	t.pending = append(t.pending[:0], t.pending[index:]...)
	if len(t.pending) > maxPendingEscape {
		t.pending = t.pending[:0]
	}
	return reply
}

// Replay returns the mode sequences a newly attached client needs so its
// terminal matches what the Agent believes it configured.
func (t *terminalObserver) Replay() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out strings.Builder
	for _, name := range replayOrder {
		if sequence, ok := t.modes[name]; ok {
			out.WriteString(sequence)
		}
	}
	return out.String()
}

// KittyFlags reports the keyboard-protocol flags the Agent asked for, which is
// what the Host answers a query with when no client can.
func (t *terminalObserver) KittyFlags() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.kittyFlags
}

func (t *terminalObserver) apply(sequence []byte, attached bool) []byte {
	final := sequence[len(sequence)-1]
	body := string(sequence[2 : len(sequence)-1])
	switch final {
	case 'h', 'l':
		if strings.HasPrefix(body, "?") {
			t.applyDECModes(strings.TrimPrefix(body, "?"), final == 'h', string(sequence))
		}
	case 'u':
		return t.applyKeyboard(body, attached)
	case 'm':
		t.applyModifiers(body)
	}
	return nil
}

// applyKeyboard handles the kitty keyboard protocol: `CSI > flags u` pushes
// flags, `CSI < u` pops them, `CSI = flags ; mode u` sets them, and `CSI ? u`
// asks which flags are in effect. The Agent's last request is what the Host
// answers with, because forwarding a push to nobody and then answering "no
// support" would leave the Agent and the terminal disagreeing.
func (t *terminalObserver) applyKeyboard(body string, attached bool) []byte {
	switch {
	case strings.HasPrefix(body, ">"):
		if flags, err := strconv.Atoi(strings.TrimPrefix(body, ">")); err == nil {
			t.setKittyFlags(flags)
		}
	case strings.HasPrefix(body, "<"):
		t.setKittyFlags(0)
	case strings.HasPrefix(body, "="):
		fields := strings.Split(strings.TrimPrefix(body, "="), ";")
		if len(fields) == 2 {
			flags, flagErr := strconv.Atoi(fields[0])
			mode, modeErr := strconv.Atoi(fields[1])
			if flagErr == nil && modeErr == nil {
				switch mode {
				case 1:
					t.setKittyFlags(flags)
				case 2:
					t.setKittyFlags(t.kittyFlags | flags)
				case 3:
					t.setKittyFlags(t.kittyFlags &^ flags)
				}
			}
		}
	case body == "?":
		if !attached {
			return []byte(fmt.Sprintf("\x1b[?%du", t.kittyFlags))
		}
	}
	return nil
}

// setKittyFlags records the flags and, for replay, uses the absolute set form
// rather than a push: replaying a push on every attach would grow the client
// terminal's mode stack for the lifetime of the session.
func (t *terminalObserver) setKittyFlags(flags int) {
	t.kittyFlags = flags
	if flags > 0 {
		t.modes["keyboard"] = fmt.Sprintf("\x1b[=%d;1u", flags)
		return
	}
	delete(t.modes, "keyboard")
}

// applyModifiers handles xterm's modifyOtherKeys (`CSI > 4 ; N m`), which is
// what Pi falls back to when the terminal reports no kitty support.
func (t *terminalObserver) applyModifiers(body string) {
	if !strings.HasPrefix(body, ">") {
		return
	}
	fields := strings.Split(strings.TrimPrefix(body, ">"), ";")
	if len(fields) < 2 || fields[0] != "4" {
		return
	}
	if fields[1] == "0" {
		delete(t.modes, "modifyOtherKeys")
		return
	}
	t.modes["modifyOtherKeys"] = "\x1b[>4;2m"
}

func (t *terminalObserver) applyDECModes(params string, enabled bool, sequence string) {
	for _, field := range strings.Split(params, ";") {
		value, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		switch value {
		case 1:
			t.setMode("appcursor", "\x1b[?1h", enabled)
		case 1004:
			t.setMode("focus", "\x1b[?1004h", enabled)
		case 2004:
			t.setMode("paste", "\x1b[?2004h", enabled)
		case 1000, 1002, 1003, 1006, 1015:
			// Mouse reporting is enabled one mode at a time, and turned off as a
			// set, so the enabled sequences accumulate and any disable clears
			// them all.
			if !enabled {
				delete(t.modes, "mouse")
				continue
			}
			if !strings.Contains(t.modes["mouse"], sequence) {
				t.modes["mouse"] += sequence
			}
		}
	}
}

func (t *terminalObserver) setMode(name, sequence string, enabled bool) {
	if !enabled {
		delete(t.modes, name)
		return
	}
	t.modes[name] = sequence
}
