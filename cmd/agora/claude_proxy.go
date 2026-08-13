package main

import (
	"context"
	"fmt"

	"github.com/delve8/agora/internal/proxy"
)

func runClaudeProxy(args []string) error {
	code, err := proxy.Run(context.Background(), proxy.DefaultOptions(args))
	if err != nil {
		return proxyExitError{code: code, err: err}
	}
	if code != 0 {
		return proxyExitError{code: code}
	}
	return nil
}

type proxyExitError struct {
	code int
	err  error
}

func (e proxyExitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("claude proxy exited with status %d", e.code)
}

func (e proxyExitError) ExitCode() int { return e.code }
