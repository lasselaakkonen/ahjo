// Package main: //go:generate hooks for the in-container ahjo-mirror daemon.
//
// The daemon is Linux-only (runs inside the container). The host CLI embeds
// arch-matched copies via internal/ahjoruntime/feature/, both for the
// ahjo-runtime Feature install path AND for the on-demand `incus file push`
// self-heal path.
//
// CI gates on `go generate ./... && git diff --exit-code`, so a stale embed
// fails the build with a non-empty diff before merge. The directives below
// invoke `go build` directly (no `make` round-trip) so the build dependency
// graph stays acyclic. The version stamp is computed inline via `git
// describe` so the host CLI's reconcile flow can compare stamps. Note: we
// must avoid `$VAR` references at the go-generate level — go generate
// substitutes them at parse time, leaving `$()` subshells intact (which is
// why the version is computed inside the same -ldflags string).
//
// The `generate` build tag keeps this file out of the daemon binary itself.

//go:build generate

package main

//go:generate sh -c "GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)\" -o ../../internal/ahjoruntime/feature/ahjo-mirror.linux-arm64 ."
//go:generate sh -c "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)\" -o ../../internal/ahjoruntime/feature/ahjo-mirror.linux-amd64 ."
