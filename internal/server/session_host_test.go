package server

import (
	"fmt"
	"os"
	"testing"

	"github.com/delve8/agora/internal/sessionhost"
)

// TestMain lets this package's binary act as `agora session-host`, so tests can
// create real hosted sessions: SessionHostRegistry.Spawn re-executes the test
// binary with `session-host --config <path>`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "session-host" {
		os.Exit(runSessionHostProcess(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func runSessionHostProcess(args []string) int {
	configPath := ""
	for index := 0; index < len(args); index++ {
		if args[index] == "--config" && index+1 < len(args) {
			configPath = args[index+1]
		}
	}
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "session-host test helper: --config is required")
		return 2
	}
	sessionhost.IgnoreTerminalSignals()
	config, err := sessionhost.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	host, err := sessionhost.New(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	if err := host.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	return 0
}
