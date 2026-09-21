package main

import (
	"fmt"
	"os"
	"strings"
)

// Build metadata, injected at link time:
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-01-01T00:00:00Z" ./cmd/agora
//
// The same values are printed by `agora version`, reported to the Server when a
// Daemon registers, and served at /api/version, which is what lets
// `agora update --check` tell what is installed from what the Server publishes.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildVersion is the version reported everywhere. AGORA_VERSION overrides the
// injected value, so a container or `make start` can label a build without a
// rebuild.
func buildVersion() string {
	if value := strings.TrimSpace(os.Getenv("AGORA_VERSION")); value != "" {
		return value
	}
	return version
}

func buildInfo() (string, string, string) {
	return buildVersion(), commit, date
}

// runVersion answers what this binary is. --porcelain prints just the version,
// which is what `agora update --check` and scripts ask the installed binary for.
func runVersion(args []string) error {
	for _, arg := range args {
		switch arg {
		case "--porcelain":
			fmt.Fprintln(os.Stdout, buildVersion())
			return nil
		case "-h", "--help":
			fmt.Fprintln(os.Stdout, "usage: agora version [--porcelain]")
			return nil
		default:
			return fmt.Errorf("unknown option %q for `agora version`", arg)
		}
	}
	current, revision, built := buildInfo()
	fmt.Fprintf(os.Stdout, "agora %s\n", current)
	fmt.Fprintf(os.Stdout, "  commit: %s\n", revision)
	fmt.Fprintf(os.Stdout, "  built:  %s\n", built)
	return nil
}
