# shellcheck shell=bash
#
# e2e/lib.sh — sourced helpers for ahjo's attended host-tier test harness.
#
# Guiding principle: run real `ahjo` commands, then validate the result against
# ground truth via incus/git/shell — never trust ahjo's own stdout or
# registry.toml as proof. Every assertion here reaches past ahjo into the
# substrate it claims to have changed.
#
# Source this from a script, then call (in order):
#   resolve_ahjo            # locate + smoke-test the binary under test
#   setup_isolation         # relocate state (Linux), arm `trap teardown EXIT`
#   ... real ahjo commands interleaved with assert_* checks ...
# Teardown is automatic on EXIT (success, failure, or Ctrl-C).
#
# Platform: on native Linux everything runs locally; on macOS the darwin `ahjo`
# relays into a Lima VM, so *validation* commands are wrapped with `incusq`
# (limactl shell "$AHJO_VM" -- …) to reach where incus actually lives. `ahjo`
# itself is always invoked directly — the launcher does its own relaying.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration knobs (override via the environment before sourcing).
# ---------------------------------------------------------------------------

# The directory the operator invoked the script from. Scripts `cd` into e2e/
# to source this file, so a RELATIVE AHJO_BIN (e.g. ./ahjo) must be resolved
# against here, not against e2e/. Each script sets this before its `cd`; the
# default covers a direct `source lib.sh`.
AHJO_E2E_PWD="${AHJO_E2E_PWD:-$PWD}"

# The VM name to relay validation into on macOS. Ignored on Linux.
AHJO_VM="${AHJO_VM:-ahjo}"

# Private sandbox repo. Added over HTTPS+PAT (a bare owner/repo alias clones
# over HTTPS when a PAT is in hand — see repoSource.cloneURL). Should carry a
# lockfile so stack-detection + warm-install are exercised.
AHJO_E2E_REPO="${AHJO_E2E_REPO:-lasselaakkonen/ahjo-e2e-sandbox}"

# Branch container `lifecycle.sh` creates off the default branch.
AHJO_E2E_BRANCH="${AHJO_E2E_BRANCH:-e2e-test-branch}"

# A tool the sandbox's lockfile should cause warm-install to provide (e.g. a
# go.sum → the go stack installs `go`). Asserted present after `repo add`.
AHJO_E2E_WARM_TOOL="${AHJO_E2E_WARM_TOOL:-go}"

# Whether to assert a per-repo GH_TOKEN landed as container env. True when the
# operator pastes a PAT at the `repo add` prompt (the default lifecycle path);
# set to 0 if you intend to run with --yes / no PAT.
AHJO_E2E_EXPECT_GH_TOKEN="${AHJO_E2E_EXPECT_GH_TOKEN:-1}"

# Ports / mirror target used by the forward/expose/mirror checkpoints.
AHJO_E2E_FWD_PORT="${AHJO_E2E_FWD_PORT:-8000}"
AHJO_E2E_EXPOSE_PORT="${AHJO_E2E_EXPOSE_PORT:-3000}"

# Additional aliases exercised via `--as` on `repo add` / `create`. Must be
# valid aliases (registry.ValidateAlias: letters, digits and any of . _ - / @,
# no leading/trailing punctuation) and must not collide with the auto-derived
# primary aliases.
AHJO_E2E_REPO_AS="${AHJO_E2E_REPO_AS:-ahjo-e2e-sandbox-alt}"
AHJO_E2E_BRANCH_AS="${AHJO_E2E_BRANCH_AS:-e2e-branch-alt}"

# A second branch `create`d from an explicit `--base` ref, to prove --base
# plumbs a ref through. AHJO_E2E_BASE_REF defaults (resolved at runtime once the
# default branch is known) to origin/<default>~1, so the checkout lands on a
# commit DIFFERENT from the default tip — the sandbox's default branch must have
# ≥2 commits. Override to pin a tag/SHA/branch (e.g. a known tag).
AHJO_E2E_BRANCH2="${AHJO_E2E_BRANCH2:-e2e-base-branch}"
AHJO_E2E_BASE_REF="${AHJO_E2E_BASE_REF:-}"

# A non-default branch that EXISTS on the sandbox's remote, used to exercise
# `repo add --default-base`. Unset → that checkpoint is skipped: we can't know
# the sandbox's branches, and pointing it at the detected default would prove
# nothing (the override must differ from auto-detection to be observable).
AHJO_E2E_ALT_BRANCH="${AHJO_E2E_ALT_BRANCH:-}"

# ---------------------------------------------------------------------------
# Derived names (deterministic from the alias — mirrors internal/registry).
# These assume `repo add` lands the un-suffixed base slug (no `-N` collision
# suffix). setup_isolation's pre-clean establishes that precondition by
# sweeping any leftover sandbox container before the run — see sweep_sandbox.
# ---------------------------------------------------------------------------

# slugify mirrors registry.sanitizeSlug: lowercase, runs of anything outside
# [a-z0-9-] collapsed to a single '-', leading/trailing '-' trimmed.
slugify() {
	printf '%s' "$1" | tr '[:upper:]' '[:lower:]' |
		sed -E 's/[^a-z0-9-]+/-/g; s/^-+//; s/-+$//'
}

REPO_ALIAS="$AHJO_E2E_REPO"
REPO_SLUG="$(slugify "$REPO_ALIAS")"
REPO_CONTAINER="ahjo-${REPO_SLUG}"                  # registry.ContainerName(slug)
BRANCH_ALIAS="${REPO_ALIAS}@${AHJO_E2E_BRANCH}"     # registry.MakeBranchAlias
BRANCH_SLUG="$(slugify "${REPO_SLUG}-${AHJO_E2E_BRANCH}")"  # registry.MakeSlug
BRANCH_CONTAINER="ahjo-${BRANCH_SLUG}"

# Second branch (the `create --base` checkpoint). Same repo, so its container
# still sits under the sandbox slug prefix → covered by teardown's targeted
# sweep without any extra bookkeeping.
BRANCH2_ALIAS="${REPO_ALIAS}@${AHJO_E2E_BRANCH2}"
BRANCH2_SLUG="$(slugify "${REPO_SLUG}-${AHJO_E2E_BRANCH2}")"
BRANCH2_CONTAINER="ahjo-${BRANCH2_SLUG}"

# The targeted-teardown prefix. Covers the repo container (ahjo-<slug>) and
# every branch container (ahjo-<slug>-…). safe_sweep refuses anything that
# isn't at least this specific, so the global ahjo-base/ahjo-osbase images and
# the user's real `ahjo-*` containers are never in scope.
SANDBOX_SLUG_PREFIX="ahjo-${REPO_SLUG}"

# Set by setup_isolation on Linux so teardown knows the throwaway HOME is safe
# to rm -rf. Never set on macOS (the in-VM ~/.ahjo is a shared singleton).
AHJO_E2E_ISOLATED_HOME=""

# ---------------------------------------------------------------------------
# Logging.
# ---------------------------------------------------------------------------

if [ -t 1 ]; then
	_C_BOLD=$'\033[1m'; _C_DIM=$'\033[2m'; _C_RED=$'\033[31m'
	_C_GRN=$'\033[32m'; _C_YEL=$'\033[33m'; _C_RST=$'\033[0m'
else
	_C_BOLD=""; _C_DIM=""; _C_RED=""; _C_GRN=""; _C_YEL=""; _C_RST=""
fi

section() { printf '\n%s== %s ==%s\n' "$_C_BOLD" "$*" "$_C_RST"; }
step()    { printf '%s→ %s%s\n' "$_C_BOLD" "$*" "$_C_RST"; }
pass()    { printf '%s  ✓ %s%s\n' "$_C_GRN" "$*" "$_C_RST"; }
note()    { printf '%s    %s%s\n' "$_C_DIM" "$*" "$_C_RST"; }
warn()    { printf '%s  ! %s%s\n' "$_C_YEL" "$*" "$_C_RST" >&2; }

# fail <message> [details]. Prints the failing assertion and any captured
# validation output, then exits non-zero (which fires `trap teardown EXIT`).
fail() {
	printf '%s  ✗ %s%s\n' "$_C_RED" "$1" "$_C_RST" >&2
	if [ "${2:-}" != "" ]; then
		printf '%s--- validation output ---\n%s\n-------------------------%s\n' \
			"$_C_DIM" "$2" "$_C_RST" >&2
	fi
	exit 1
}

# ---------------------------------------------------------------------------
# Platform wrapper.
# ---------------------------------------------------------------------------

# incusq: run a validation command where incus actually lives. Linux: locally.
# macOS: inside the Lima VM. Used for every `incus …` / in-container `git …`
# assertion — NOT for `ahjo` itself (the launcher relays on its own).
incusq() {
	if [ "$(uname)" = Darwin ]; then
		limactl shell "$AHJO_VM" -- "$@"
	else
		"$@"
	fi
}

# cexec <container> [args…]: `incus exec` as root inside the container.
cexec() { incusq incus exec "$1" -- "${@:2}"; }

# cgit <container> [git-args…]: run git inside /repo as uid 1000 (the owner —
# git refuses a "dubiously owned" tree from another uid), mirroring how ahjo's
# own code invokes in-container git.
cgit() { incusq incus exec "$1" --user 1000 --cwd /repo -- git "${@:2}"; }

# ---------------------------------------------------------------------------
# Binary under test + preflight.
# ---------------------------------------------------------------------------

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

# resolve_ahjo locates the working-tree build and smoke-tests it. AHJO_BIN may
# be relative on entry; it's resolved to an absolute path so a later cd can't
# strip it.
resolve_ahjo() {
	require_cmd jq
	: "${AHJO_BIN:?set AHJO_BIN to the ahjo binary under test (e.g. AHJO_BIN=./ahjo)}"
	# A relative AHJO_BIN is relative to where the operator ran the script, not
	# to e2e/ (scripts cd here to source lib.sh). Anchor it before checking.
	case "$AHJO_BIN" in
		/*) : ;;
		*) AHJO_BIN="$AHJO_E2E_PWD/$AHJO_BIN" ;;
	esac
	if [ ! -x "$AHJO_BIN" ]; then
		fail "AHJO_BIN is not an executable: $AHJO_BIN (run \`make build\`?)"
	fi
	AHJO_BIN="$(cd "$(dirname "$AHJO_BIN")" && pwd)/$(basename "$AHJO_BIN")"
	local ver
	ver="$("$AHJO_BIN" --version 2>&1)" || fail "\`$AHJO_BIN --version\` failed" "$ver"
	note "ahjo binary: $AHJO_BIN ($ver)"

	# Confirm validation reaches incus on whichever platform we're on.
	if [ "$(uname)" = Darwin ]; then
		require_cmd limactl
	fi
	local out
	out="$(incusq incus list --format=csv -c n 2>&1)" ||
		fail "cannot reach incus via incusq (is the daemon / Lima VM up?)" "$out"
}

# ahjo: the single call site for the binary under test. Always invoked directly
# (never through incusq) — the macOS launcher relays into the VM itself.
ahjo() { "$AHJO_BIN" "$@"; }

# ---------------------------------------------------------------------------
# Isolation & teardown.
# ---------------------------------------------------------------------------

# setup_isolation relocates host-side state (Linux only) and arms teardown.
#
# Linux: a throwaway HOME reroutes all of ~/.ahjo + ~/.ahjo-shared (every path
# roots at os.UserHomeDir via paths.home()), leaving the real state untouched;
# a fresh HOME self-bootstraps its host SSH keys on first `repo add`. The
# global incus state (ahjo-base/ahjo-osbase images, subuid/subgid) is reused.
#
# macOS: the in-VM ~/.ahjo is a shared singleton, so a HOME override on the Mac
# doesn't isolate it. Isolation there rests entirely on the unique sandbox slug
# + the `repo rm` / targeted-sweep teardown below. AHJO_HOST_HOME is left
# untouched so the claude-config push still reads the operator's real Mac home.
setup_isolation() {
	if [ "$(uname)" != Darwin ]; then
		local newhome
		newhome="$(mktemp -d /tmp/ahjo-e2e.XXXXXX)"
		export HOME="$newhome"
		AHJO_E2E_ISOLATED_HOME="$newhome"
		note "isolated HOME: $HOME  (real ~/.ahjo untouched)"
	else
		note "macOS: no HOME isolation (in-VM ~/.ahjo is shared); relying on"
		note "       sandbox slug '$SANDBOX_SLUG_PREFIX' + repo-rm teardown"
	fi
	# shellcheck disable=SC2064  # capture current values into the trap now.
	trap teardown EXIT
	note "teardown armed: repo rm --force + targeted sweep of $SANDBOX_SLUG_PREFIX*"

	# Pre-clean: a prior run that never reached its EXIT teardown (Ctrl-C during
	# teardown, kill -9, crash) can leave a sandbox container behind. On macOS the
	# in-VM ~/.ahjo + containers are a shared singleton; even on Linux the
	# throwaway HOME resets only the registry, not the global incus containers. A
	# surviving `ahjo-<slug>` container makes the next `repo add` suffix its slug
	# to `-2` (its collision probe walks past the orphan — see
	# registry.AllocateRepoSlug), which would point every derived *_CONTAINER name
	# above at the stale leftover instead of what this run creates. Sweep up front
	# so the run starts from the clean slate those names assume.
	section "pre-clean — clear any leftover sandbox state"
	sweep_sandbox
}

# teardown removes everything this run created and nothing else. NEVER calls
# `ahjo nuke` (that deletes the global ahjo-base/ahjo-osbase images — see
# internal/cli/nuke.go). Best-effort throughout: a failed step logs and the
# next still runs.
teardown() {
	local code=$?
	trap - EXIT
	section "teardown"
	# 1+2. Hand the repo back to ahjo, then targeted-sweep the leftovers.
	sweep_sandbox
	# 3. Linux only: drop the throwaway HOME.
	if [ -n "$AHJO_E2E_ISOLATED_HOME" ] && [ -d "$AHJO_E2E_ISOLATED_HOME" ]; then
		step "rm -rf $AHJO_E2E_ISOLATED_HOME"
		rm -rf "$AHJO_E2E_ISOLATED_HOME" || warn "rm -rf HOME failed"
	fi
	if [ "$code" -eq 0 ]; then
		printf '%s\nALL CHECKS PASSED%s\n' "$_C_GRN" "$_C_RST"
	else
		printf '%s\nRUN FAILED (exit %d) — substrate swept, real state untouched%s\n' \
			"$_C_RED" "$code" "$_C_RST" >&2
	fi
}

# sweep_sandbox clears all state for this run's sandbox repo, then sweeps any
# residue. Shared by the pre-run clean (setup_isolation) and teardown so
# "clean slate" means exactly the same thing going in and coming out.
#  1. `ahjo repo rm --force` hands the repo back to ahjo: it drops registry
#     rows, frees the SSH port, deletes the repo + every branch container,
#     reverts a live mirror, and tail-sweeps suffix-past-orphan leftovers. When
#     nothing is registered (the common pre-clean / post-success case) it exits
#     non-zero — expected, not a failure — so we only note and press on.
#  2. A targeted incus sweep catches anything ahjo lost track of (a half-built
#     container from a crashed/cancelled add, a branch the registry forgot).
sweep_sandbox() {
	step "ahjo repo rm $REPO_ALIAS --force"
	"$AHJO_BIN" repo rm "$REPO_ALIAS" --force || note "repo rm: nothing registered to remove (sweeping incus directly)"
	safe_sweep "$SANDBOX_SLUG_PREFIX"
}

# safe_sweep <prefix>: incus delete --force every container matching <prefix>
# or <prefix>-…, after asserting <prefix> is at least as specific as the
# sandbox slug. The guard is the load-bearing safety net: it refuses a bare
# `ahjo-` (or anything shorter / not under the sandbox slug) so a typo can
# never enumerate the user's real containers.
safe_sweep() {
	local prefix="$1"
	case "$prefix" in
		"$SANDBOX_SLUG_PREFIX"*) : ;;
		*) fail "safe_sweep refused unsafe prefix '$prefix' (must start with '$SANDBOX_SLUG_PREFIX')" ;;
	esac
	if [ "$prefix" = "ahjo-" ] || [ "${#prefix}" -le 5 ]; then
		fail "safe_sweep refused too-broad prefix '$prefix'"
	fi
	local names out
	out="$(incusq incus list --format=json 2>&1)" || { warn "sweep: incus list failed: $out"; return 0; }
	names="$(printf '%s' "$out" | jq -r --arg p "$prefix" \
		'.[] | .name | select(. == $p or startswith($p + "-"))')"
	[ -n "$names" ] || { note "sweep: nothing matching $prefix*"; return 0; }
	local n
	while IFS= read -r n; do
		[ -n "$n" ] || continue
		step "incus delete --force $n"
		incusq incus delete --force "$n" || warn "delete $n failed"
	done <<<"$names"
}

# ---------------------------------------------------------------------------
# Ground-truth assertions. Each reaches past ahjo into incus/git/shell and
# fails loudly (dumping the validation output) on mismatch.
# ---------------------------------------------------------------------------

# _status <container> → "Running"/"Stopped"/"" (absent). Echoes; caller asserts.
_status() {
	local out
	out="$(incusq incus list --format=json "$1" 2>&1)" || { printf '__ERR__\n%s' "$out"; return 0; }
	printf '%s' "$out" | jq -r --arg n "$1" '.[] | select(.name==$n) | .status'
}

assert_container_running() {
	local s; s="$(_status "$1")"
	[ "$s" = "Running" ] || fail "container $1 expected Running, got '${s:-<absent>}'" "$s"
	pass "container $1 is Running"
}

assert_container_stopped() {
	local s; s="$(_status "$1")"
	[ "$s" = "Stopped" ] || fail "container $1 expected Stopped, got '${s:-<absent>}'" "$s"
	pass "container $1 is Stopped"
}

assert_container_absent() {
	local s; s="$(_status "$1")"
	[ -z "$s" ] || fail "container $1 expected absent, got '$s'"
	pass "container $1 is absent"
}

# assert_repo_at_branch <container> <branch>: /repo's checked-out branch matches.
# Reads .git/HEAD via `incus file pull` rather than `git rev-parse`, so it works
# on a STOPPED container too — `repo add` leaves the repo container stopped (the
# COW source), and `incus exec` needs a running container.
assert_repo_at_branch() {
	local out ref
	out="$(incusq incus file pull "$1/repo/.git/HEAD" - 2>&1)" ||
		fail "read /repo/.git/HEAD from $1 failed" "$out"
	# Detached HEAD would be a raw SHA, not "ref: refs/heads/<branch>".
	ref="$(printf '%s' "$out" | sed -nE 's#^ref: refs/heads/(.*)$#\1#p' | tr -d '[:space:]')"
	[ "$ref" = "$2" ] || fail "container $1 /repo HEAD is '${ref:-$out}', expected branch '$2'" "$out"
	pass "container $1 /repo is on branch $2"
}

# assert_repo_clean <container>: no uncommitted changes in /repo.
assert_repo_clean() {
	local out
	out="$(cgit "$1" status --porcelain 2>&1)" || fail "git status in $1 failed" "$out"
	[ -z "$out" ] || fail "container $1 /repo is dirty" "$out"
	pass "container $1 /repo is clean"
}

# assert_repo_synced_with_origin <container> <branch>: local HEAD == origin/<branch>
# (the invariant a successful `repo pull --ff-only` establishes).
assert_repo_synced_with_origin() {
	local head origin
	head="$(cgit "$1" rev-parse HEAD 2>&1)" || fail "rev-parse HEAD in $1 failed" "$head"
	origin="$(cgit "$1" rev-parse "origin/$2" 2>&1)" || fail "rev-parse origin/$2 in $1 failed" "$origin"
	head="$(printf '%s' "$head" | tr -d '[:space:]')"
	origin="$(printf '%s' "$origin" | tr -d '[:space:]')"
	[ "$head" = "$origin" ] || fail "container $1 HEAD ($head) != origin/$2 ($origin)" "$head vs $origin"
	pass "container $1 HEAD is in sync with origin/$2 ($head)"
}

# _device_show <container> → `incus config device show` output (or __ERR__).
_device_show() {
	local out
	out="$(incusq incus config device show "$1" 2>&1)" || { printf '__ERR__\n%s' "$out"; return 0; }
	printf '%s' "$out"
}

assert_device_present() {
	local out; out="$(_device_show "$1")"
	printf '%s\n' "$out" | grep -qE "^${2}:" || fail "device $2 absent on $1" "$out"
	pass "device $2 present on $1"
}

assert_device_absent() {
	local out; out="$(_device_show "$1")"
	if printf '%s\n' "$out" | grep -qE "^${2}:"; then
		fail "device $2 still present on $1" "$out"
	fi
	pass "device $2 absent on $1"
}

# assert_proxy_device <container> <name> [listen] [connect]: device exists and
# (when given) its listen=/connect= lines match. Pass "" to skip a side — e.g.
# the host port of `ahjo-ssh` is dynamic, so only connect= is pinned.
assert_proxy_device() {
	local c="$1" name="$2" listen="${3:-}" connect="${4:-}" out
	out="$(_device_show "$c")"
	printf '%s\n' "$out" | grep -qE "^${name}:" || fail "proxy device $name absent on $c" "$out"
	if [ -n "$listen" ]; then
		printf '%s\n' "$out" | grep -qE "^[[:space:]]+listen: ${listen}$" ||
			fail "proxy $name on $c: listen != '$listen'" "$out"
	fi
	if [ -n "$connect" ]; then
		printf '%s\n' "$out" | grep -qE "^[[:space:]]+connect: ${connect}$" ||
			fail "proxy $name on $c: connect != '$connect'" "$out"
	fi
	pass "proxy device $name on $c (listen='${listen:-*}' connect='${connect:-*}')"
}

# assert_container_env <container> <KEY>: the container carries `environment.<KEY>`
# config with a non-empty value — the exact mechanism installRepoToken uses for
# GH_TOKEN/GITHUB_TOKEN (ConfigSet "environment.GH_TOKEN"). Read via
# `incus config get`, which is ground truth past ahjo's stdout and works on a
# stopped container (so it covers the repo container right after `repo add`),
# and which `incus copy` carries into branch containers.
#
# NOTE: attach-time forward_env vars (CLAUDE_CODE_OAUTH_TOKEN) are injected only
# by the shell/claude attach and are NOT container config — they won't show here.
# Eyeball those inside `ahjo shell` via `printenv` (lifecycle step 6).
assert_container_env() {
	local out
	out="$(incusq incus config get "$1" "environment.$2" 2>&1)" ||
		fail "read environment.$2 config from $1 failed" "$out"
	[ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ] ||
		fail "container $1 has no environment.$2 (empty/unset)" "$out"
	pass "container $1 carries environment.$2"
}

# assert_tool_present <container> <bin>: resolvable on PATH, or present at the
# usual sbin locations (covers sshd, which lives in /usr/sbin).
assert_tool_present() {
	local out
	out="$(cexec "$1" sh -lc "command -v '$2' || test -x /usr/sbin/'$2' || test -x /sbin/'$2'" 2>&1)" ||
		fail "tool $2 not found in $1" "$out"
	pass "tool $2 present in $1"
}

# assert_unit_active <container> <unit>: systemd reports the unit active.
assert_unit_active() {
	local out
	out="$(cexec "$1" systemctl is-active "$2" 2>&1 || true)"
	[ "$(printf '%s' "$out" | tr -d '[:space:]')" = active ] ||
		fail "unit $2 on $1 is '$out', expected active" "$out"
	pass "unit $2 active on $1"
}

# assert_port_answers <hostport>: a TCP connect to host 127.0.0.1:<port>
# succeeds. Host-side (the loopback where `expose` publishes via Lima
# auto-forward on macOS, or directly on Linux). Optional in the lifecycle —
# only meaningful when a service is actually listening behind the proxy.
assert_port_answers() {
	local p="$1"
	if command -v nc >/dev/null 2>&1; then
		nc -z -w 3 127.0.0.1 "$p" 2>/dev/null ||
			fail "nothing answering on host 127.0.0.1:$p"
	elif command -v curl >/dev/null 2>&1; then
		curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$p/" ||
			fail "nothing answering on host 127.0.0.1:$p"
	else
		warn "neither nc nor curl available; skipping port-answers check for $p"
		return 0
	fi
	pass "host 127.0.0.1:$p answers"
}

# assert_mirror_target_populated <dir>: the host mirror target has at least one
# mirrored file (ignoring .git/, which the mirror never propagates).
assert_mirror_target_populated() {
	local dir="$1" count
	[ -d "$dir" ] || fail "mirror target $dir does not exist"
	count="$(find "$dir" -mindepth 1 -not -path '*/.git/*' -not -name '.git' -type f 2>/dev/null | head -1)"
	[ -n "$count" ] || fail "mirror target $dir has no mirrored files"
	pass "mirror target $dir is populated"
}

# assert_mirror_propagates <container> <hostdir>: write a unique probe file
# INSIDE the container's /repo (as the uid-1000 owner, mimicking a real edit)
# and assert the in-container ahjo-mirror daemon pushes it out to the host
# target with identical content. This exercises the live watch→push path, not
# just the activation-time bootstrap copy. Records the probe's basename in
# REPLY_MIRROR_PROBE so a later `mirror off --no-revert` check can confirm the
# file was kept. The host target is read directly (it's the disk device's source
# on the host — Linux fs, or the Mac via virtiofs), like the bootstrap check.
assert_mirror_propagates() {
	local c="$1" dir="$2"
	local probe="ahjo-e2e-mirror-probe-$$-${RANDOM}"
	step "write $probe into $c:/repo, expect it mirrored to $dir"
	incusq incus exec "$c" --user 1000 -- sh -c "printf '%s\n' '$probe' > /repo/$probe" ||
		fail "could not write mirror probe into $c:/repo"
	local i
	for i in $(seq 1 30); do
		if [ -f "$dir/$probe" ] && grep -qF "$probe" "$dir/$probe" 2>/dev/null; then
			REPLY_MIRROR_PROBE="$probe"
			pass "mirror propagated the live edit $probe → $dir (content matches)"
			return 0
		fi
		sleep 1
	done
	fail "mirror did not propagate $probe to $dir within 30s (live watch broken?)" "$(ls -la "$dir" 2>&1)"
}

# assert_ssh_attaches <branch-alias>: `ahjo ssh <alias>` connects over the
# generated ssh-config and runs a command as the in-container `ubuntu` user.
# The remote command is fed on stdin (no TTY → ssh runs it non-interactively and
# exits); stderr (MOTD/banner) is dropped so stdout is just the command output.
# StrictHostKeyChecking=yes against the pre-seeded known_hosts means a clean
# connect already proves we reached THIS container's sshd, not some local
# listener. `ahjo ssh` is invoked directly (never through incusq) — like every
# ahjo call, the launcher relays on its own. Needs the container running with
# sshd wired (create/shell establish that).
assert_ssh_attaches() {
	local alias="$1" out rc=0
	out="$(printf 'id -un\n' | ahjo ssh "$alias" 2>/dev/null)" || rc=$?
	[ "$rc" -eq 0 ] || fail "ahjo ssh $alias exited $rc (ssh connect/auth failed?)" "$out"
	# The remote login shell prints the dynamic MOTD on stdout *before* our
	# command's output, so match a standalone `ubuntu` line (the `id -un` result)
	# rather than the whole capture — the banner has no bare `ubuntu` line.
	printf '%s\n' "$out" | grep -qx ubuntu ||
		fail "ahjo ssh $alias: no standalone 'ubuntu' line in remote 'id -un' output" "$out"
	pass "ahjo ssh $alias attached over ssh and ran as the container's ubuntu user"
}

# assert_repo_head_matches <container> <ref>: /repo's HEAD resolves to the same
# commit as <ref> (e.g. origin/main~1). Proves `create --base <ref>` checked the
# branch out from exactly that ref. Needs a running container (uses cgit).
assert_repo_head_matches() {
	local c="$1" ref="$2" head want
	head="$(cgit "$c" rev-parse HEAD 2>&1)" || fail "rev-parse HEAD in $c failed" "$head"
	want="$(cgit "$c" rev-parse "$ref" 2>&1)" || fail "rev-parse $ref in $c failed" "$want"
	head="$(printf '%s' "$head" | tr -d '[:space:]')"
	want="$(printf '%s' "$want" | tr -d '[:space:]')"
	[ "$head" = "$want" ] || fail "container $c HEAD ($head) != $ref ($want)" "$head vs $want"
	pass "container $c HEAD is at $ref ($head)"
}

# assert_alias_maps <alias>: the alias appears in the generated alias maps under
# ~/.ahjo-shared (the `aliases` / `repo-aliases` files ahjo writes for the
# cross-host shim). Grepping these is ground truth past ahjo's own stdout — it
# proves an `--as` alias was actually registered. The shared dir is host-visible
# on both platforms (Linux: under the isolated HOME; macOS: <mac-home>/.ahjo-shared
# via virtiofs), so this reads it directly rather than through incusq.
assert_alias_maps() {
	local alias="$1"
	local f="$HOME/.ahjo-shared/aliases" rf="$HOME/.ahjo-shared/repo-aliases"
	if grep -qE "^${alias}[[:space:]]" "$f" 2>/dev/null ||
		grep -qE "^${alias}[[:space:]]" "$rf" 2>/dev/null; then
		pass "alias '$alias' present in the generated alias map"
	else
		fail "alias '$alias' not found in $f or $rf" "$(cat "$f" "$rf" 2>/dev/null)"
	fi
}

# ---------------------------------------------------------------------------
# Operator interaction (attended steps).
# ---------------------------------------------------------------------------

# confirm <question>: y/N prompt, returns 0 on yes. Defaults to no on a
# non-TTY (the harness is meant to be run attended).
confirm() {
	local ans
	if [ ! -t 0 ]; then return 1; fi
	printf '%s? %s [y/N] %s' "$_C_BOLD" "$*" "$_C_RST"
	read -r ans || return 1
	case "$ans" in [yY]*) return 0 ;; *) return 1 ;; esac
}

# operator_check <question>: a qualitative checkpoint the operator answers.
# A "no" fails the run (something the substrate assertions can't see — e.g. a
# TUI rendered, claude actually launched — didn't hold).
operator_check() {
	if confirm "$*"; then
		pass "operator confirmed: $*"
	else
		fail "operator reported failure: $*"
	fi
}

# prompt_enter <message>: pause for the operator to read/act before continuing.
prompt_enter() {
	[ -t 0 ] || return 0
	printf '%s%s — press Enter to continue%s' "$_C_DIM" "$*" "$_C_RST"
	read -r _ || true
}
