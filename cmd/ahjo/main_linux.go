//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lasselaakkonen/ahjo/internal/cli"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

var version = "dev"

func main() {
	if err := tokenstore.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: warning: load ~/.ahjo/.env:", err)
	}

	// Cancel the command's context on the first Ctrl-C / SIGTERM so
	// long-running ops (image build, repo-add wiring, WaitReady polls)
	// unwind instead of hanging; a second signal then kills the process
	// hard (NotifyContext restores default handling after the first).
	// cmd.Context() in every RunE handler derives from this.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRoot(version)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
