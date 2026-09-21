package terminal

// ModesReset turns off the terminal modes an interactive Agent enables for
// itself: bracketed paste, the kitty keyboard protocol, xterm's modifyOtherKeys,
// mouse reporting, focus events and application cursor keys.
//
// An attach client has to send this when it leaves. The modes belong to the
// session, and a client that is detached (or whose Agent crashed) would
// otherwise leave the user's shell reporting pastes and modified keys in a
// protocol nothing is reading.
const ModesReset = "\x1b[?2004l\x1b[<u\x1b[>4;0m" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l\x1b[?1004l\x1b[?1l"
