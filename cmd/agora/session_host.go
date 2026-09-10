package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/delve8/agora/internal/sessionhost"
)

func runSessionHost(args []string) error {
	flags := flag.NewFlagSet("session-host", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the JSON session-host launch config")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("usage: agora session-host --config <path>")
	}
	config, err := sessionhost.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load session-host config: %w", err)
	}
	host, err := sessionhost.New(config)
	if err != nil {
		return err
	}
	defer os.Remove(*configPath)
	// A Host process owns the Agent's lifetime, so terminal signals aimed at the
	// Daemon must not end it. Spawn also detaches the Host into its own session;
	// both measures together keep "Daemon restart" from meaning "Agent restart".
	sessionhost.IgnoreTerminalSignals()
	if err := host.Run(); err != nil {
		return fmt.Errorf("session-host: %w", err)
	}
	return nil
}
