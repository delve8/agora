package main

import (
	"context"
	"fmt"
	"os"

	"github.com/delve8/agora/internal/proxy"
)

func main() {
	code, err := proxy.Run(context.Background(), proxy.DefaultOptions(os.Args[1:]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[agora-proxy] %v\n", err)
	}
	os.Exit(code)
}
