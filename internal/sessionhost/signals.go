package sessionhost

import (
	"os/signal"
	"syscall"
)

// IgnoreTerminalSignals makes the current process survive the terminal signals
// that belong to whoever started the Daemon.
//
// A Host owns an Agent's lifetime, so stopping the Daemon is not an Agent stop.
// SIGINT (Ctrl-C in the Daemon's terminal) and SIGHUP (that terminal closing)
// are delivered to the Daemon's foreground process group, and Spawn additionally
// detaches the Host into its own session. Ignoring them here means neither path
// can end a Host that is still responsible for a running Agent.
//
// Only an explicit stop/shutdown command, SIGTERM aimed at the Host itself, or
// the Agent's own exit ends the process. Host entrypoints must call this before
// Run; the library does not change a caller's signal disposition implicitly, so
// in-process users (and tests) keep their own signal handling.
func IgnoreTerminalSignals() {
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP)
}
