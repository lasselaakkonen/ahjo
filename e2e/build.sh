#!/usr/bin/env bash
#
# e2e/build.sh — OPT-IN. Rebuilds the operator's real global ahjo-base image and
# asserts both embedded Features materialized into it (validation of the shared
# Materialize helper + per-feature de-dup, commit 7637376).
#
#   make build && AHJO_BIN=./ahjo bash e2e/build.sh
#
# WARNING: this runs `ahjo update -y`, which REBUILDS ahjo-base in your real
# incus image store (it does NOT touch ahjo-osbase beyond an idempotent
# re-pull). Every subsequent container you build will come from the rebuilt
# image. Skip this script if you don't want that.
#
# Validation is done by launching a throwaway container straight from the
# freshly-built ahjo-base and `command -v`-ing the tools each embedded Feature's
# install.sh lays down — testing the image update just produced, not a
# derivative, and needing no PAT/auth. The probe is named under the sandbox slug
# so the standard teardown sweep reclaims it.

cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

PROBE_CONTAINER="${SANDBOX_SLUG_PREFIX}-probe"

# Tools the two embedded Features install (see internal/ahjodevtools/feature/
# install.sh and internal/ahjoruntime/feature/install.sh). httpie's binary is
# `http`; ast-grep installs only `ast-grep` (not `sg`); fd is symlinked from
# fdfind. sshd lives in /usr/sbin (assert_tool_present covers that).
DEVTOOLS=(rg fd eza yq ast-grep http make)
RUNTIME_BINS=(claude ahjo-claude-prepare ahjo-mirror sshd)

main() {
	resolve_ahjo
	section "OPT-IN: this rebuilds your real ahjo-base image"
	if ! confirm "proceed with \`ahjo update -y\` (rebuilds ahjo-base)"; then
		note "aborted by operator"
		exit 0
	fi
	setup_isolation

	step "ahjo update -y  (rebuild ahjo-base via the devcontainer Feature pipeline)"
	ahjo update -y

	probe_ahjo_base
}

# probe_ahjo_base launches a container directly from the rebuilt ahjo-base,
# waits for it to accept exec, and asserts every embedded-Feature tool resolves.
probe_ahjo_base() {
	section "probe ahjo-base — embedded Features materialized"
	# Clean any leftover probe from a prior aborted run.
	incusq incus delete --force "$PROBE_CONTAINER" >/dev/null 2>&1 || true

	step "incus launch ahjo-base $PROBE_CONTAINER"
	local out
	out="$(incusq incus launch ahjo-base "$PROBE_CONTAINER" 2>&1)" ||
		fail "incus launch ahjo-base failed (did update publish the image?)" "$out"

	# Wait until the container accepts exec (init far enough along for command -v).
	local i ready=0
	for i in $(seq 1 30); do
		if incusq incus exec "$PROBE_CONTAINER" -- true >/dev/null 2>&1; then ready=1; break; fi
		sleep 1
	done
	[ "$ready" = 1 ] || fail "$PROBE_CONTAINER never accepted exec after launch"

	local t
	step "ahjo-devtools Feature tools"
	for t in "${DEVTOOLS[@]}"; do assert_tool_present "$PROBE_CONTAINER" "$t"; done
	step "ahjo-runtime Feature bits"
	for t in "${RUNTIME_BINS[@]}"; do assert_tool_present "$PROBE_CONTAINER" "$t"; done

	step "incus delete --force $PROBE_CONTAINER"
	incusq incus delete --force "$PROBE_CONTAINER" || warn "delete $PROBE_CONTAINER failed (teardown will sweep it)"
}

main "$@"
