//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lasselaakkonen/ahjo/internal/cli"
	"github.com/lasselaakkonen/ahjo/internal/relayenv"
)

var version = "dev"

func main() {
	// The Mac shim relays secrets (the per-repo GH_TOKEN) through a 0600
	// file under ~/.ahjo-shared/.relay-env and passes only its basename.
	// Consume it into the process env first: tokenstore.Lookup prefers a
	// non-empty process env value over ~/.ahjo/.env, so the relayed value
	// wins.
	if name := os.Getenv(relayenv.EnvVar); name != "" {
		_ = os.Unsetenv(relayenv.EnvVar)
		pairs, err := relayenv.Consume(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ahjo: warning: relay env:", err)
		}
		for k, v := range pairs {
			os.Setenv(k, v)
		}
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
