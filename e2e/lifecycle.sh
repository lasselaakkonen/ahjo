#!/usr/bin/env bash
#
# e2e/lifecycle.sh — the main attended lifecycle run.
#
# Drives the real container-lifecycle flows end to end against the sandbox repo,
# validating each against ground truth (incus/git/shell). Reuses the existing
# global ahjo-base; never rebuilds it (see build.sh for that). Teardown is
# automatic on exit.
#
#   make build && AHJO_BIN=./ahjo bash e2e/lifecycle.sh
#
# You will be prompted live for: a GitHub PAT (repo scope) at `repo add`, the
# stack-detection prompt (accept it so warm-install runs), and Claude auth at
# `ahjo claude` if not already signed in. See e2e/README.md for prereqs.

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

# Skip the mirror checkpoint (it is the most Mac-centric flow) with
# AHJO_E2E_SKIP_MIRROR=1.
AHJO_E2E_SKIP_MIRROR="${AHJO_E2E_SKIP_MIRROR:-0}"

main() {
	resolve_ahjo
	setup_isolation
	note "repo:   $REPO_ALIAS  → container $REPO_CONTAINER"
	note "branch: $BRANCH_ALIAS → container $BRANCH_CONTAINER"

	step_repo_add
	step_create
	step_forward
	step_expose
	step_mirror
	step_ls_top
	step_shell
	step_claude
	step_repo_pull
	step_repo_rm
}

# 1. repo add (live PAT + stack prompt). Validates the repoAddSetup phases:
#    container built + stopped, /repo cloned at the default branch, ahjo-ssh
#    proxy wired, the per-repo GH_TOKEN promoted to container env config.
step_repo_add() {
	section "1. repo add — $REPO_ALIAS"
	note "When prompted: paste a GitHub PAT (repo scope), then ACCEPT the"
	note "detected stack so warm-install runs (the warm tool is checked later)."
	prompt_enter "ready to run \`ahjo repo add $REPO_ALIAS\`"
	ahjo repo add "$REPO_ALIAS"

	assert_container_stopped "$REPO_CONTAINER"
	# Capture the default branch from the stopped container's .git/HEAD; reused
	# by the repo-pull sync check.
	DEFAULT_BRANCH="$(incusq incus file pull "$REPO_CONTAINER/repo/.git/HEAD" - 2>/dev/null |
		sed -nE 's#^ref: refs/heads/(.*)$#\1#p' | tr -d '[:space:]')"
	[ -n "$DEFAULT_BRANCH" ] || fail "could not read default branch from $REPO_CONTAINER /repo/.git/HEAD"
	note "default branch detected: $DEFAULT_BRANCH"
	assert_repo_at_branch "$REPO_CONTAINER" "$DEFAULT_BRANCH"
	assert_proxy_device "$REPO_CONTAINER" "ahjo-ssh" "" "tcp:127.0.0.1:22"
	if [ "$AHJO_E2E_EXPECT_GH_TOKEN" = 1 ]; then
		assert_container_env "$REPO_CONTAINER" GH_TOKEN
	else
		note "AHJO_E2E_EXPECT_GH_TOKEN=0 — skipping GH_TOKEN env check"
	fi
}

# 2. create <branch>. Validates the COW clone: branch container running, /repo
#    checked out on the new branch, ahjo-ssh re-wired with the branch's port,
#    the warm-installed tool inherited via reflink, and GH_TOKEN carried over by
#    `incus copy`.
step_create() {
	section "2. create — $AHJO_E2E_BRANCH"
	ahjo create "$REPO_ALIAS" "$AHJO_E2E_BRANCH"

	assert_container_running "$BRANCH_CONTAINER"
	assert_repo_at_branch "$BRANCH_CONTAINER" "$AHJO_E2E_BRANCH"
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-ssh" "" "tcp:127.0.0.1:22"
	# Warm-install / Feature de-dup landed in the COW-inherited tree. Runs here
	# (not on the stopped repo container) because `command -v` needs `incus
	# exec`, which needs a running container.
	assert_tool_present "$BRANCH_CONTAINER" "$AHJO_E2E_WARM_TOOL"
	if [ "$AHJO_E2E_EXPECT_GH_TOKEN" = 1 ]; then
		assert_container_env "$BRANCH_CONTAINER" GH_TOKEN
	fi
}

# 3. forward <host-port> [→ container]. bind=container proxy that pipes the host
#    port into the container; the listen socket lives on the container's
#    127.0.0.1:<port>. `--off` removes it.
step_forward() {
	section "3. forward — host :$AHJO_E2E_FWD_PORT → container"
	ahjo forward "$BRANCH_ALIAS" "$AHJO_E2E_FWD_PORT"
	# connect= is the host gateway IP (Lima) or 127.0.0.1 (native); only the
	# in-container listen socket is deterministic, so pin that.
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-forward-$AHJO_E2E_FWD_PORT" \
		"tcp:127.0.0.1:$AHJO_E2E_FWD_PORT" ""
	ahjo forward "$BRANCH_ALIAS" "$AHJO_E2E_FWD_PORT" --off
	assert_device_absent "$BRANCH_CONTAINER" "ahjo-forward-$AHJO_E2E_FWD_PORT"
}

# 4. expose <container-port>. Publishes a container port to the host loopback
#    (Lima auto-forward on macOS). connect= is pinned to the container port; the
#    host listen port is allocated dynamically, so it isn't asserted here.
#    (`expose` has no `--off`; the device is reclaimed at teardown / on stop.)
step_expose() {
	section "4. expose — container :$AHJO_E2E_EXPOSE_PORT → host"
	ahjo expose "$BRANCH_ALIAS" "$AHJO_E2E_EXPOSE_PORT"
	assert_proxy_device "$BRANCH_CONTAINER" "ahjo-expose-$AHJO_E2E_EXPOSE_PORT" \
		"" "tcp:127.0.0.1:$AHJO_E2E_EXPOSE_PORT"
	note "(optional) start a listener on container :$AHJO_E2E_EXPOSE_PORT to also"
	note "exercise assert_port_answers against the published host port."
}

# 5. mirror on/off. Disk device + ahjo-mirror unit, then the host target is
#    actually populated by the daemon's bootstrap sync; `off` tears it down.
step_mirror() {
	section "5. mirror — /repo → $AHJO_E2E_MIRROR_DIR"
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

	ahjo mirror off
	assert_device_absent "$BRANCH_CONTAINER" "mirror"
}

# 6. ls + top — operator eyeballs the registry view and the TUI.
step_ls_top() {
	section "6. ls + top (operator eyeball)"
	ahjo ls || true
	operator_check "did \`ahjo ls\` list $BRANCH_ALIAS with a running container"
	prompt_enter "next: \`ahjo top\` opens the TUI — look around, then press q to quit"
	ahjo top || true
	operator_check "did the \`ahjo top\` TUI render and quit cleanly on q"
}

# 7. shell — interactive attach. Operator corroborates the attach + env
#    forwarding; the harness separately asserts the config-level GH_TOKEN.
step_shell() {
	section "7. shell — $BRANCH_ALIAS (interactive)"
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

# 8. claude — live launch (auth in-browser if needed). Qualitative only.
step_claude() {
	section "8. claude — $BRANCH_ALIAS (interactive)"
	prompt_enter "next: \`ahjo claude $BRANCH_ALIAS\` — confirm claude launches, then exit"
	ahjo claude "$BRANCH_ALIAS" || true
	operator_check "did \`claude\` launch inside the container"
}

# 9. repo pull — fast-forward the default-branch (COW source) container against
#    origin. Validates it ends running and in sync with origin/<default>.
step_repo_pull() {
	section "9. repo pull — $REPO_ALIAS"
	ahjo repo pull "$REPO_ALIAS"
	assert_container_running "$REPO_CONTAINER"
	assert_repo_synced_with_origin "$REPO_CONTAINER" "$DEFAULT_BRANCH"
}

# 10. repo rm --force — tear down repo + every branch container.
step_repo_rm() {
	section "10. repo rm --force — $REPO_ALIAS"
	ahjo repo rm "$REPO_ALIAS" --force
	assert_container_absent "$BRANCH_CONTAINER"
	assert_container_absent "$REPO_CONTAINER"
}

main "$@"
