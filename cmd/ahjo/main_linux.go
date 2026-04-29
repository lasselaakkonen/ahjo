//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/lasselaakkonen/ahjo/internal/cli"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

var version = "dev"

func main() {
	if err := tokenstore.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: warning: load ~/.ahjo/.env:", err)
	}
	root := cli.NewRoot(version)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
