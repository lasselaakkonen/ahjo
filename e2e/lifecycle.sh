#!/usr/bin/env bash
#
# e2e/lifecycle.sh — the main attended lifecycle run.
#
# Drives the real container-lifecycle flows end to end against the sandbox repo,
# validating each against ground truth (incus/git/ssh/shell). Reuses the existing
# global ahjo-base; never rebuilds it (see build.sh for that). Teardown is
# automatic on exit.
#
#   make build && AHJO_BIN=./ahjo bash e2e/lifecycle.sh
#
# You will be prompted live for: a GitHub PAT (repo scope) at `repo add`, the
# stack-detection prompt (accept it so warm-install runs), Claude auth at
# `ahjo claude` if not already signed in, and an IDE choice at `ahjo ide` if you
# have more than one installed. The opt-in `--default-base` checkpoint (only when
# AHJO_E2E_ALT_BRANCH is set) re-prompts for a PAT unless GH_TOKEN is exported.
# See e2e/README.md for prereqs.

# Guard the pre-source statements (notably the cd) too: lib.sh sets this, but
# only once sourced — a failed cd would otherwise fall through to sourcing
# lib.sh from the wrong directory unguarded.
set -euo pipefail

# Capture the invocation dir before cd so a relative AHJO_BIN (e.g. ./ahjo)
# resolves against where the operator ran this, not e2e/.
AHJO_E2E_PWD="${AHJO_E2E_PWD:-$PWD}"
cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

# Host mirror target. On Linux this lands in the throwaway HOME (outside
# ~/.ahjo, which validateMirrorTarget requires); on macOS it sits under the
# real Mac home (the writable virtiofs window mirror needs). Per-slug so a
# stale run can't collide.
AHJO_E2E_MIRROR_DIR="${AHJO_E2E_MIRROR_DIR:-$HOME/ahjo-e2e-mirror-$BRANCH_SLUG}"

# extra_teardown (teardown hook in lib.sh): drop the mirror target. It lives in
# the real Mac home (outside the swept substrate), and `mirror off --no-revert`
# deliberately leaves files behind — so a mid-run failure between `mirror on`
# and the inline cleanup would otherwise leave a non-empty, non-git dir that
# makes the next run's `mirror on` take the no-snapshot ModeFreshNonEmpty path.
extra_teardown() {
	rm -rf "$AHJO_E2E_MIRROR_DIR" 2>/dev/null || true
}

# Skip the mirror checkpoint (it is the most Mac-centric flow) with
# AHJO_E2E_SKIP_MIRROR=1.
AHJO_E2E_SKIP_MIRROR="${AHJO_E2E_SKIP_MIRROR:-0}"

main() {
	resolve_ahjo
	setup_isolation
	note "repo:   $REPO_ALIAS  → container $REPO_CONTAINER"
	note "branch: $BRANCH_ALIAS → container $BRANCH_CONTAINER"

	step_env
	step_repo_add
	step_create
	step_doctor
	step_gc
	step_forward
	step_expose
	step_mirror
	step_ls_top
	step_shell
	step_ssh
	step_claude
	step_ide
	step_create_base
	step_rm_branch
	step_repo_pull
	step_set_token
	step_repo_rm
	step_default_base
}

# 1. env set/get/unset/list — host-side KEY=VALUE config CRUD (no container).
#    Runs first: it only touches ~/.ahjo/.env (the isolated HOME on Linux, the
#    in-VM home on macOS), so it's a fast smoke of the binary's config path
#    before any container work. Uses an inert probe key so it can't perturb the
#    forwarded keys (GH_TOKEN / CLAUDE_CODE_OAUTH_TOKEN).
step_env() {
	section "1. env — host-side config CRUD (no container)"
	local key=AHJO_E2E_PROBE val=probe-value-1234
	ahjo env set "$key" "$val"
	local got
	got="$(ahjo env get "$key")" || fail "ahjo env get $key failed after set"
	[ "$got" = "$val" ] || fail "ahjo env get $key returned '$got', expected '$val'"
	pass "env set/get round-trips ($key)"
	# Default `list` masks the value (only the last 4 chars show); --show reveals
	# it raw. Grep the FULL value to detect a leak — the masked line still ends
	# in the last 4 chars, so a substring match would false-positive.
	if ! ahjo env list | grep -qE "^${key}="; then
		fail "ahjo env list did not list $key"
	fi
	if ahjo env list | grep -qF "$val"; then
		fail "ahjo env list leaked the raw value (should be masked)"
	fi
	pass "env list masks the value"
	if ! ahjo env list --show | grep -qF "${key}=${val}"; then
		fail "ahjo env list --show did not reveal ${key}=${val}"
	fi
	pass "env list --show reveals the raw value"
	ahjo env unset "$key"
	if ahjo env get "$key" >/dev/null 2>&1; then
		fail "ahjo env get $key still succeeds after unset"
	fi
	pass "env unset removed $key"
}

# 2. repo add (live PAT + stack prompt), with --as. Validates the repoAddSetup
#    phases: container built + stopped, /repo cloned at the default branch,
#    ahjo-ssh proxy wired, the per-repo GH_TOKEN promoted to container env, and
#    the extra --as alias landing in the generated alias map.
step_repo_add() {
	section "2. repo add — $REPO_ALIAS (--as $AHJO_E2E_REPO_AS)"
	note "When prompted: paste a GitHub PAT (repo scope), then ACCEPT the"
	note "detected stack so warm-install runs (the warm tool is checked later)."
	prompt_enter "ready to run \`ahjo repo add $REPO_ALIAS --as $AHJO_E2E_REPO_AS\`"
	ahjo repo add "$REPO_ALIAS" --as "$AHJO_E2E_REPO_AS"

	assert_container_stopped "$REPO_CONTAINER"
	# Capture the default branch from the stopped container's .git/HEAD; reused
	# by the repo-pull sync check and the create --base ref.
	DEFAULT_BRANCH="$(incusq incus file pull "$REPO_CONTAINER/repo/.git/HEAD" - 2>/dev/null |
		sed -nE 's#^ref: refs/heads/(.*)$#\1#p' | tr -d '[:space:]')"
	[ -n "$DEFAULT_BRANCH" ] || fail "could not read default branch from $REPO_CONTAINER /repo/.git/HEAD"
	note "default branch detected: $DEFAULT_BRANCH"
	assert_repo_at_branch "$REPO_CONTAINER" "$DEFAULT_BRANCH"
	assert_proxy_device "$REPO_CONTAINER" "ahjo-ssh" "" "tcp:127.0.0.1:22"
	# The --as alias must land in the generated alias map (ground truth ahjo
	# writes for the cross-host shim); it's also used functionally as the repo
	# handle in step_create_base.
	assert_alias_maps "$AHJO_E2E_REPO_AS"
	if [ "$AHJO_E2E_EXPECT_GH_TOKEN" = 1 ]; then
		assert_container_env "$REPO_CONTAINER" GH_TOKEN
	else
		note "AHJO_E2E_EXPECT_GH_TOKEN=0 — skipping GH_TOKEN env check"
	fi
	# repo ls must list the freshly-registered repo — a ground-truth read of the
	# registry the rest of the run depends on. The --as alias is in r.Aliases, so
	# the joined alias column carries it.
	ahjo repo ls | grep -qF "$AHJO_E2E_REPO_AS" ||
		fail "ahjo repo ls did not list the --as alias $AHJO_E2E_REPO_AS"
	pass "ahjo repo ls lists $AHJO_E2E_REPO_AS"
}

# 3. create <branch>, with --as. Validates the COW clone: branch container
#    running, /repo checked out on the new branch, ahjo-ssh re-wired with the
#    branch's port, the warm-installed tool inherited via reflink, GH_TOKEN
#    carried over by `incus copy`, and the extra --as branch alias registered.
step_create() {
	section "3. create — $AHJO_E2E_BRANCH (--as $AHJO_E2E_BRANCH_AS)"
	ahjo create "$REPO_ALIAS" "$AHJO_E2E_BRANCH" --as "$AHJO_E2E_BRANCH_AS"

	assert_container_running "$BRANCH_CONTAINER"
	assert_repo_at_branch "$BRANCH_CONTAINER" "$AHJO_E2E_BRANCH"
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-ssh" "" "tcp:127.0.0.1:22"
	# Warm-install / Feature de-dup landed in the COW-inherited tree. Runs here
	# (not on the stopped repo container) because `command -v` needs `incus
	# exec`, which needs a running container.
	assert_tool_present "$BRANCH_CONTAINER" "$AHJO_E2E_WARM_TOOL"
	# The --as alias resolves to this branch — proven again functionally by
	# `ahjo ssh $AHJO_E2E_BRANCH_AS` in step_ssh.
	assert_alias_maps "$AHJO_E2E_BRANCH_AS"
	if [ "$AHJO_E2E_EXPECT_GH_TOKEN" = 1 ]; then
		assert_container_env "$BRANCH_CONTAINER" GH_TOKEN
	fi
}

# 3.1 doctor — read-only environment diagnostics. Driver coverage for `ahjo
#     doctor`, which the harness otherwise never exercised. Runs after repo add +
#     create so a real repo is registered and a live container exists for the
#     per-repo survey. doctor legitimately exits non-zero in the isolated HOME
#     (no CLAUDE_CODE_OAUTH_TOKEN / host git identity there), so we don't gate on
#     its exit code — we assert it reached its incus-backed checks: the `incus`
#     binary probe and the ahjo-base image-alias lookup (the base is present
#     because `ahjo init` is a prerequisite). On macOS the relayed in-VM block
#     carries these same lines, so both assertions hold on either platform.
step_doctor() {
	section "3.1 doctor — environment diagnostics (read-only)"
	local out
	out="$(ahjo doctor 2>&1 || true)"
	[ -n "$out" ] || fail "ahjo doctor produced no output"
	printf '%s\n' "$out" | grep -qF "incus on PATH" ||
		fail "ahjo doctor did not report the incus binary check" "$out"
	printf '%s\n' "$out" | grep -qF "ahjo-base image present" ||
		fail "ahjo doctor did not confirm the ahjo-base image (incus image-alias check)" "$out"
	pass "ahjo doctor ran its incus-backed checks (incus binary + ahjo-base image)"
}

# 3.2 gc — stale-branch reporting. Driver coverage for `ahjo gc`. The default
#     24h window must NOT flag the seconds-old branch; `--older-than 0` must flag
#     it as a candidate but, without `--prune`, only REPORT (dry run). The branch
#     container still running afterward is the safety check that report mode
#     never deletes. (gc always excludes the repo's default/COW-source branch.)
#
#     gc scans the GLOBAL registry across every repo, so the operator's own
#     unrelated stale branches may legitimately appear. We therefore assert on
#     the e2e branch alias specifically rather than on the whole report being
#     empty — the seconds-old branch must be absent under the default window and
#     present under --older-than 0.
step_gc() {
	section "3.2 gc — stale-branch report (no prune)"
	local out
	out="$(ahjo gc 2>&1)" || fail "ahjo gc (default window) failed" "$out"
	if printf '%s\n' "$out" | grep -qF "$BRANCH_ALIAS"; then
		fail "ahjo gc (default 24h) flagged the seconds-old branch $BRANCH_ALIAS" "$out"
	fi
	pass "ahjo gc (default 24h) does not flag the fresh branch"

	out="$(ahjo gc --older-than 0 2>&1)" || fail "ahjo gc --older-than 0 failed" "$out"
	printf '%s\n' "$out" | grep -qF "$BRANCH_ALIAS" ||
		fail "ahjo gc --older-than 0 did not list the branch $BRANCH_ALIAS" "$out"
	printf '%s\n' "$out" | grep -q "dry run" ||
		fail "ahjo gc --older-than 0 did not stay in report mode (no 'dry run' notice)" "$out"
	pass "ahjo gc --older-than 0 reports the branch as a dry-run candidate"

	# Report mode must not delete: the branch container must survive untouched.
	assert_container_running "$BRANCH_CONTAINER"
}

# 4. forward <host-port> [→ container]. bind=container proxy that pipes the host
#    port into the container; the listen socket lives on the container's
#    127.0.0.1:<port>. `--off` removes it.
step_forward() {
	section "4. forward — host :$AHJO_E2E_FWD_PORT → container"
	ahjo forward "$BRANCH_ALIAS" "$AHJO_E2E_FWD_PORT"
	# connect= is the host gateway IP (Lima) or 127.0.0.1 (native); only the
	# in-container listen socket is deterministic, so pin that.
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-forward-$AHJO_E2E_FWD_PORT" \
		"tcp:127.0.0.1:$AHJO_E2E_FWD_PORT" ""
	ahjo forward "$BRANCH_ALIAS" "$AHJO_E2E_FWD_PORT" --off
	assert_device_absent "$BRANCH_CONTAINER" "ahjo-forward-$AHJO_E2E_FWD_PORT"
}

# 5. expose <container-port>. Publishes a container port to the host loopback
#    (Lima auto-forward on macOS). connect= is pinned to the container port; the
#    host listen port is allocated dynamically, so it isn't asserted here.
#    (`expose` has no `--off`; the device is reclaimed at teardown / on stop.)
step_expose() {
	section "5. expose — container :$AHJO_E2E_EXPOSE_PORT → host"
	ahjo expose "$BRANCH_ALIAS" "$AHJO_E2E_EXPOSE_PORT"
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-expose-$AHJO_E2E_EXPOSE_PORT" \
		"" "tcp:127.0.0.1:$AHJO_E2E_EXPOSE_PORT"
	note "(optional) start a listener on container :$AHJO_E2E_EXPOSE_PORT to also"
	note "exercise assert_port_answers against the published host port."

	# expose --sync reconciles auto-expose proxy devices to the container's live
	# TCP loopback listeners; by contract it NEVER touches a MANUAL expose entry.
	# With no guaranteed in-container listener we assert the invariant we can:
	# --sync runs cleanly and leaves the manual device created above intact.
	local sout
	sout="$(ahjo expose "$BRANCH_ALIAS" --sync 2>&1)" || fail "ahjo expose --sync failed" "$sout"
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-expose-$AHJO_E2E_EXPOSE_PORT" \
		"" "tcp:127.0.0.1:$AHJO_E2E_EXPOSE_PORT"
	pass "ahjo expose --sync ran and preserved the manual expose device"
}

# 6. mirror on/off + live propagation. Disk device + ahjo-mirror unit, the host
#    target populated by the daemon's bootstrap sync, then — the real test — a
#    file modified INSIDE the container is observed landing on the host via the
#    daemon's watch→push path. `mirror status` is grepped for the active mirror
#    and `mirror logs` captured briefly. Finally `off --no-revert` must LEAVE the
#    mirrored probe in place (a plain `off` would revert the target instead).
step_mirror() {
	section "6. mirror — /repo → $AHJO_E2E_MIRROR_DIR"
	if [ "$AHJO_E2E_SKIP_MIRROR" = 1 ]; then
		note "AHJO_E2E_SKIP_MIRROR=1 — skipping mirror checkpoint"
		return 0
	fi
	mkdir -p "$AHJO_E2E_MIRROR_DIR"
	ahjo mirror "$BRANCH_ALIAS" --target "$AHJO_E2E_MIRROR_DIR"
	assert_device_present "$BRANCH_CONTAINER" "mirror"
	assert_unit_active "$BRANCH_CONTAINER" "ahjo-mirror.service"
	# The daemon bootstrap-copies /repo on activation; poll the host target.
	local i
	for i in $(seq 1 30); do
		if find "$AHJO_E2E_MIRROR_DIR" -mindepth 1 -not -path '*/.git/*' -type f 2>/dev/null | head -1 | grep -q .; then
			break
		fi
		sleep 1
	done
	assert_mirror_target_populated "$AHJO_E2E_MIRROR_DIR"

	# `mirror status` should report this container's mirror as active.
	local st
	st="$(ahjo mirror status 2>&1)" || fail "ahjo mirror status failed" "$st"
	printf '%s\n' "$st" | grep -q "$BRANCH_CONTAINER" ||
		fail "ahjo mirror status did not list $BRANCH_CONTAINER" "$st"
	# Anchored so "mirror: inactive" can't satisfy a bare "active" match.
	printf '%s\n' "$st" | grep -qE '^mirror: active' ||
		fail "ahjo mirror status did not report an active mirror" "$st"
	pass "ahjo mirror status reports $BRANCH_CONTAINER active"

	# Live edit inside the container → must appear on the host target.
	assert_mirror_propagates "$BRANCH_CONTAINER" "$AHJO_E2E_MIRROR_DIR"

	# `mirror logs` tails `journalctl -u ahjo-mirror --follow` and replaces the
	# process, so capture a short window in the BACKGROUND rather than attaching —
	# attaching would need an operator Ctrl-C, which would also signal this
	# script. Best-effort: a quiet daemon may emit little, so thin/empty output
	# only warns.
	local logf
	logf="$(mktemp)"
	ahjo mirror logs "$BRANCH_ALIAS" </dev/null >"$logf" 2>&1 &
	local lpid=$!
	sleep 3
	kill -TERM "$lpid" 2>/dev/null || true
	wait "$lpid" 2>/dev/null || true
	if grep -qiE 'ahjo-mirror|mirror|systemd|journal' "$logf"; then
		pass "ahjo mirror logs streamed the ahjo-mirror journal"
	else
		warn "ahjo mirror logs produced no recognizable output (daemon may be quiet)"
	fi
	rm -f "$logf"

	# off --no-revert: stop the mirror but KEEP the mirrored files. The probe we
	# wrote is a mirror-added file, so a plain `off` would revert (remove) it;
	# --no-revert must leave it on the host.
	ahjo mirror off --no-revert
	assert_device_absent "$BRANCH_CONTAINER" "mirror"
	if [ -z "${REPLY_MIRROR_PROBE:-}" ] || [ ! -f "$AHJO_E2E_MIRROR_DIR/$REPLY_MIRROR_PROBE" ]; then
		fail "mirror off --no-revert did not keep the mirrored probe (${REPLY_MIRROR_PROBE:-<unset>}) on the host"
	fi
	pass "mirror off --no-revert kept the mirrored files in place"

	# mirror revert <target> — the orphan-recovery path (no container needed).
	# `off --no-revert` deliberately kept both the mirrored files AND the
	# pre-mirror snapshot (decideRevert returns false under --no-revert, so the
	# snapshot is never consumed). With the device already torn down, `mirror
	# revert` auto-resolves that single snapshot and restores the target to its
	# pre-mirror state — so the probe --no-revert just kept must now be GONE.
	ahjo mirror revert "$AHJO_E2E_MIRROR_DIR"
	if [ -n "${REPLY_MIRROR_PROBE:-}" ] && [ -f "$AHJO_E2E_MIRROR_DIR/$REPLY_MIRROR_PROBE" ]; then
		fail "mirror revert did not remove the kept probe ($REPLY_MIRROR_PROBE) from $AHJO_E2E_MIRROR_DIR"
	fi
	pass "mirror revert restored the target (removed the mirror-added probe)"

	# Remove the in-container probe so the branch returns to a clean /repo (it's
	# otherwise an untracked file the operator would see at the shell step). The
	# mirror is already detached, so this never reaches the host target.
	if [ -n "${REPLY_MIRROR_PROBE:-}" ]; then
		incusq incus exec "$BRANCH_CONTAINER" --user 1000 -- rm -f "/repo/$REPLY_MIRROR_PROBE" 2>/dev/null || true
	fi
	# The throwaway target is dropped by extra_teardown (the teardown hook), so a
	# failure anywhere in this step can't leave a non-empty dir behind to poison
	# the next run's `mirror on`.
}

# 7. ls + top — operator eyeballs the registry view and the TUI.
step_ls_top() {
	section "7. ls + top (operator eyeball)"
	ahjo ls || true
	operator_check "did \`ahjo ls\` list $BRANCH_ALIAS with a running container"
	prompt_enter "next: \`ahjo top\` opens the TUI — look around, then press q to quit"
	ahjo top || true
	operator_check "did the \`ahjo top\` TUI render and quit cleanly on q"
}

# 8. shell — interactive attach. Operator corroborates the attach + env
#    forwarding; the harness separately asserts the config-level GH_TOKEN.
step_shell() {
	section "8. shell — $BRANCH_ALIAS (interactive)"
	cat <<EOF
Inside the shell, please run:
    git status
    printenv CLAUDE_CODE_OAUTH_TOKEN
    printenv GH_TOKEN
    exit
EOF
	prompt_enter "next: \`ahjo shell $BRANCH_ALIAS\`"
	ahjo shell "$BRANCH_ALIAS" || true
	operator_check "did the shell attach as ubuntu in /repo, with CLAUDE_CODE_OAUTH_TOKEN set"
	# forward_env (CLAUDE_CODE_OAUTH_TOKEN) is attach-only; GH_TOKEN is config,
	# so the harness can corroborate it independently of the attach.
	if [ "$AHJO_E2E_EXPECT_GH_TOKEN" = 1 ]; then
		assert_container_env "$BRANCH_CONTAINER" GH_TOKEN
	fi
}

# 9. ssh — alternate attach path, machine-asserted. `ahjo ssh` execs
#    `ssh -F <generated-config>`; we drive it through the branch's --as alias
#    (proving that alias resolves too) and pipe a remote command on stdin so ssh
#    runs it non-interactively. The container is up (step_shell left it running).
step_ssh() {
	section "9. ssh — $AHJO_E2E_BRANCH_AS (machine-asserted attach)"
	assert_ssh_attaches "$AHJO_E2E_BRANCH_AS"
}

# 10. claude — live launch (auth in-browser if needed). Qualitative only.
step_claude() {
	section "10. claude — $BRANCH_ALIAS (interactive)"
	prompt_enter "next: \`ahjo claude $BRANCH_ALIAS\` — confirm claude launches, then exit"
	ahjo claude "$BRANCH_ALIAS" || true
	operator_check "did \`claude\` launch inside the container"
}

# 11. ide — alternate attach path (GUI). `ahjo ide` detects SSH-capable IDEs on
#     the host (Cursor, VS Code, Windsurf, Zed) and launches one, or errors
#     cleanly when none are installed — a GUI side effect, so operator-eyeball
#     like `top`/`claude`. Non-blocking (it spawns the IDE detached and returns),
#     so no Ctrl-C dance.
step_ide() {
	section "11. ide — $AHJO_E2E_BRANCH_AS (operator eyeball)"
	note "ahjo ide picks an SSH-capable IDE; with several it prompts, with none"
	note "it prints a clean 'no SSH-capable IDEs found' error — both are OK."
	prompt_enter "next: \`ahjo ide $AHJO_E2E_BRANCH_AS\` — pick an IDE if prompted, then come back"
	ahjo ide "$AHJO_E2E_BRANCH_AS" || true
	operator_check "did an IDE open against the container over ssh-remote (or ahjo report no IDE installed)"
}

# 12. create --base — a second branch from an explicit ref, addressed via the
#     repo's --as alias (so this also proves `repo add --as` yielded a usable
#     repo handle). --base defaults to origin/<default>~1 so the checkout lands
#     on a commit DIFFERENT from the default tip, proving --base plumbs the ref
#     through (sandbox default branch needs ≥2 commits; override AHJO_E2E_BASE_REF).
step_create_base() {
	section "12. create --base — $BRANCH2_ALIAS via repo alias $AHJO_E2E_REPO_AS"
	local ref="$AHJO_E2E_BASE_REF"
	[ -n "$ref" ] || ref="origin/${DEFAULT_BRANCH}~1"
	note "basing $AHJO_E2E_BRANCH2 on $ref"
	ahjo create "$AHJO_E2E_REPO_AS" "$AHJO_E2E_BRANCH2" --base "$ref"

	assert_container_running "$BRANCH2_CONTAINER"
	assert_repo_at_branch "$BRANCH2_CONTAINER" "$AHJO_E2E_BRANCH2"
	assert_repo_head_matches "$BRANCH2_CONTAINER" "$ref"
}

# 13. rm <alias> — standalone single-branch teardown. The lifecycle otherwise
#     only removes branches as a side effect of `repo rm --force`; this exercises
#     the dedicated path. Removing a non-default branch must leave the repo and
#     the other branch container untouched. (rm fires a detached base-refresh of
#     the repo container, so we assert it still EXISTS rather than pinning its
#     run state.)
step_rm_branch() {
	section "13. rm — $BRANCH2_ALIAS (single branch)"
	ahjo rm "$BRANCH2_ALIAS"
	assert_container_absent "$BRANCH2_CONTAINER"
	assert_container_running "$BRANCH_CONTAINER"
	local s
	s="$(_status "$REPO_CONTAINER")"
	[ -n "$s" ] || fail "repo container $REPO_CONTAINER vanished after a single-branch rm"
	pass "repo container $REPO_CONTAINER still present (status: $s)"
}

# 14. repo pull — fast-forward the default-branch (COW source) container against
#     origin. Validates it ends running and in sync with origin/<default>.
step_repo_pull() {
	section "14. repo pull — $REPO_ALIAS"
	ahjo repo pull "$REPO_ALIAS"
	assert_container_running "$REPO_CONTAINER"
	assert_repo_synced_with_origin "$REPO_CONTAINER" "$DEFAULT_BRANCH"
}

# 14.1 repo set-token — rotate the per-repo GitHub PAT and re-forward it onto
#      the repo's containers as environment.GH_TOKEN/GITHUB_TOKEN. Driver
#      coverage for `ahjo repo set-token`. Linux-only: the in-VM Linux path reads
#      the token from stdin and writes it to the token store + every existing
#      repo container; the macOS path instead consumes the value the shim relays
#      from the login Keychain (GH_TOKEN env), a round trip that's operator
#      territory, not machine-assertable from in-VM. Runs after `repo pull` (the
#      last token-dependent network step) and before teardown, with a
#      recognizable sentinel so overwriting the real PAT here is harmless.
step_set_token() {
	section "14.1 repo set-token — $AHJO_E2E_REPO_AS (Linux: stdin token)"
	if [ "$(uname)" = Darwin ]; then
		note "macOS: set-token consumes the relayed Keychain value; skip (operator-verified)."
		return 0
	fi
	local sentinel="ghp_e2e_settoken_${BRANCH_SLUG}_sentinel"
	printf '%s\n' "$sentinel" | ahjo repo set-token "$AHJO_E2E_REPO_AS" ||
		fail "ahjo repo set-token failed"
	# Ground truth: both names installRepoToken sets must equal the sentinel on
	# the repo container. `incus config get` reads it directly (works on the
	# stopped COW source too), past ahjo's own stdout.
	local key got
	for key in GH_TOKEN GITHUB_TOKEN; do
		got="$(incusq incus config get "$REPO_CONTAINER" "environment.$key" 2>&1 | tr -d '[:space:]')"
		[ "$got" = "$sentinel" ] ||
			fail "set-token did not forward environment.$key to $REPO_CONTAINER (got '$got', want the sentinel)"
	done
	pass "repo set-token forwarded the new token to $REPO_CONTAINER (GH_TOKEN + GITHUB_TOKEN)"
}

# 15. repo rm --force — tear down repo + every branch container.
step_repo_rm() {
	section "15. repo rm --force — $REPO_ALIAS"
	ahjo repo rm "$REPO_ALIAS" --force
	assert_container_absent "$BRANCH_CONTAINER"
	assert_container_absent "$REPO_CONTAINER"
}

# 16. repo add --default-base — opt-in second mini-cycle. Runs AFTER the main
#     repo is torn down (step 15), so the slug is free and `repo add` lands the
#     base slug again. Needs a real non-default branch on the remote
#     (AHJO_E2E_ALT_BRANCH); skipped when unset, since pointing it at the
#     detected default would prove nothing. With an explicit --default-base,
#     repo_add runs `git checkout -B <base> origin/<base>`, so the repo container
#     must end up checked out on that branch.
step_default_base() {
	section "16. repo add --default-base (opt-in)"
	if [ -z "${AHJO_E2E_ALT_BRANCH:-}" ]; then
		note "AHJO_E2E_ALT_BRANCH unset — skipping the --default-base checkpoint."
		note "Set it to a non-default branch that exists on $REPO_ALIAS's remote to run it."
		return 0
	fi
	local alt="$AHJO_E2E_ALT_BRANCH"
	note "re-adding $REPO_ALIAS with --default-base $alt (must exist on the remote)"
	if [ -n "${GH_TOKEN:-}" ]; then
		note "GH_TOKEN exported — adding unattended with --yes"
		ahjo repo add "$REPO_ALIAS" --default-base "$alt" --yes
	else
		prompt_enter "next: \`ahjo repo add $REPO_ALIAS --default-base $alt\` — paste a PAT"
		ahjo repo add "$REPO_ALIAS" --default-base "$alt"
	fi

	assert_container_stopped "$REPO_CONTAINER"
	assert_repo_at_branch "$REPO_CONTAINER" "$alt"
	pass "repo container checked out the custom --default-base ($alt)"

	step "ahjo repo rm $REPO_ALIAS --force (clean up the opt-in cycle)"
	ahjo repo rm "$REPO_ALIAS" --force || true
	assert_container_absent "$REPO_CONTAINER"
}

main "$@"
