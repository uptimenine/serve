package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/uptimenine/serve/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := cli.New(version)
	exitCode := cmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(exitCode)
}
