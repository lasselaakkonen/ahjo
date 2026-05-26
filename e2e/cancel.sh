#!/usr/bin/env bash
#
# e2e/cancel.sh — Ctrl-C / cancellation behavior (primary validation of the
# context-threading work, commit 81fb1f5: cmd.Context() into long-running ops).
#
#   make build && AHJO_BIN=./ahjo bash e2e/cancel.sh
#
# Two flows:
#   1. repo add, interrupted mid clone/warm-install — assert ahjo exits
#      PROMPTLY (the in-flight `incus exec` is torn down, not left to run to
#      completion or hang), the lockfile is released (a follow-up `ahjo ls`
#      works), and the half-built container is cleanly removable.
#   2. update, interrupted mid-build — assert prompt exit and NO wedged
#      `ahjo-build-<rand>` container (the `defer ContainerDeleteForce` in
#      internal/devcontainer/build.go).
#
# The repo-add flow runs unattended when GH_TOKEN is exported (it drives
# `--yes` against the sandbox repo so there's no live prompt to block the
# timed SIGINT); otherwise it falls back to an attended Ctrl-C.

# Capture the invocation dir before cd so a relative AHJO_BIN (e.g. ./ahjo)
# resolves against where the operator ran this, not e2e/.
AHJO_E2E_PWD="${AHJO_E2E_PWD:-$PWD}"
cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

# Seconds to let a command get into its long phase before sending SIGINT.
AHJO_E2E_CANCEL_DELAY="${AHJO_E2E_CANCEL_DELAY:-3}"
AHJO_E2E_UPDATE_CANCEL_DELAY="${AHJO_E2E_UPDATE_CANCEL_DELAY:-20}"
# Max seconds to wait for prompt exit after SIGINT before declaring a hang.
AHJO_E2E_CANCEL_DEADLINE="${AHJO_E2E_CANCEL_DEADLINE:-30}"

main() {
	resolve_ahjo
	setup_isolation
	cancel_repo_add
	cancel_update
}

# wait_for_prompt_exit <pid> <label>: after SIGINT has been sent to <pid>, poll
# until it exits, failing if it outlives the deadline (a hang — the bug this
# test guards against). Returns the process's exit code in REPLY_RC.
wait_for_prompt_exit() {
	local pid="$1" label="$2" waited=0
	while kill -0 "$pid" 2>/dev/null; do
		sleep 1; waited=$((waited + 1))
		if [ "$waited" -ge "$AHJO_E2E_CANCEL_DEADLINE" ]; then
			kill -KILL "$pid" 2>/dev/null || true
			fail "$label did not exit within ${AHJO_E2E_CANCEL_DEADLINE}s of SIGINT (hung)"
		fi
	done
	# `wait` reports the cancelled job's non-zero code; capture it without
	# tripping `set -e`.
	REPLY_RC=0
	wait "$pid" || REPLY_RC=$?
	note "$label exited ${waited}s after SIGINT with code $REPLY_RC"
}

# 1. repo add cancellation.
cancel_repo_add() {
	section "1. repo add — Ctrl-C mid clone/warm-install"
	if [ -n "${GH_TOKEN:-}" ]; then
		_cancel_repo_add_scripted
	else
		_cancel_repo_add_attended
	fi
	_assert_post_cancel_clean
}

# Unattended: GH_TOKEN exported → `--yes` clones the sandbox over HTTPS with no
# prompt, so a timed `kill -INT` exercises the cancel path without a human.
_cancel_repo_add_scripted() {
	local log; log="$(mktemp)"
	step "ahjo repo add $REPO_ALIAS --yes  (background; SIGINT after ${AHJO_E2E_CANCEL_DELAY}s)"
	ahjo repo add "$REPO_ALIAS" --yes </dev/null >"$log" 2>&1 &
	local pid=$!
	sleep "$AHJO_E2E_CANCEL_DELAY"
	if ! kill -0 "$pid" 2>/dev/null; then
		warn "repo add already finished before SIGINT (clone too fast to interrupt);"
		warn "lower AHJO_E2E_CANCEL_DELAY or use a larger sandbox to exercise the cancel"
		wait "$pid" || true
		rm -f "$log"
		return 0
	fi
	kill -INT "$pid" 2>/dev/null || true
	wait_for_prompt_exit "$pid" "repo add"
	[ "$REPLY_RC" -ne 0 ] || fail "repo add returned 0 after SIGINT (expected non-zero cancel)"
	pass "repo add exited non-zero promptly after SIGINT"
	rm -f "$log"
}

# Attended: operator runs it and presses Ctrl-C while the clone / warm-install
# is on screen.
_cancel_repo_add_attended() {
	note "No GH_TOKEN exported — attended cancel."
	note "Run completes in the foreground; press Ctrl-C while the clone or"
	note "warm-install is on screen (NOT during the PAT prompt)."
	prompt_enter "next: \`ahjo repo add $REPO_ALIAS\` — paste PAT, then Ctrl-C mid clone"
	local start rc=0
	start=$(date +%s)
	ahjo repo add "$REPO_ALIAS" || rc=$?
	local elapsed=$(( $(date +%s) - start ))
	note "repo add returned code $rc after ${elapsed}s"
	operator_check "did you press Ctrl-C mid clone/warm-install and ahjo exit promptly (not hang)"
}

# Common post-cancel assertions: the lock was released and the half-built
# container is cleanly removable (sweepUnmanagedContainers path — no registry
# row exists because the cancel preceded commitRegistry).
_assert_post_cancel_clean() {
	step "ahjo ls  (must succeed → lockfile was released)"
	ahjo ls >/dev/null || fail "ahjo ls failed after cancel (lockfile leaked?)"
	pass "ahjo ls succeeded after cancel — lock released"

	step "ahjo repo rm $REPO_ALIAS --force  (clean up any half-built orphan)"
	ahjo repo rm "$REPO_ALIAS" --force || true
	assert_container_absent "$REPO_CONTAINER"
}

# 2. update cancellation. Heavier (re-pulls ahjo-osbase, launches a transient
# build container) but cancels BEFORE the final `incus publish`, so the real
# ahjo-base image is left intact. Gated behind a confirm so an operator who
# doesn't want to touch the image store can skip it.
cancel_update() {
	section "2. update — Ctrl-C mid-build (no wedged ahjo-build-* container)"
	note "This exercises the build pipeline: it re-pulls ahjo-osbase and creates"
	note "a transient ahjo-build-<rand> container, then cancels before publish —"
	note "your real ahjo-base image is NOT rebuilt or deleted."
	if ! confirm "run the update-cancel check"; then
		note "skipped by operator"
		return 0
	fi

	local log; log="$(mktemp)"
	step "ahjo update -y  (background; SIGINT after ${AHJO_E2E_UPDATE_CANCEL_DELAY}s)"
	ahjo update -y </dev/null >"$log" 2>&1 &
	local pid=$!
	sleep "$AHJO_E2E_UPDATE_CANCEL_DELAY"
	if ! kill -0 "$pid" 2>/dev/null; then
		warn "update finished before SIGINT — raise AHJO_E2E_UPDATE_CANCEL_DELAY to"
		warn "catch it mid-build; can't validate the build-container cleanup this run"
		wait "$pid" || true
		rm -f "$log"
		return 0
	fi
	kill -INT "$pid" 2>/dev/null || true
	wait_for_prompt_exit "$pid" "update"
	[ "$REPLY_RC" -ne 0 ] || fail "update returned 0 after SIGINT (expected non-zero cancel)"
	pass "update exited non-zero promptly after SIGINT"
	rm -f "$log"

	# The defer in BuildAhjoBase force-deletes the transient build container on
	# any return, including a ctx-cancel return. Poll briefly for it to clear.
	local i builds=""
	for i in $(seq 1 10); do
		builds="$(incusq incus list --format=json 2>/dev/null |
			jq -r '.[].name | select(startswith("ahjo-build-"))')"
		[ -z "$builds" ] && break
		sleep 1
	done
	[ -z "$builds" ] || fail "wedged build container(s) survived the cancel: $builds"
	pass "no ahjo-build-* container left wedged after the cancel"
}

main "$@"
