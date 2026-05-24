#!/usr/bin/env bash
# ahjo statusline for Claude Code. Installed into the container at creation by
# internal/cli/statusline.go, unless the user already configured a statusLine of
# their own. Renders, separated by " · ":
#
#   ●<git> · <branch> · ←mirroring · exposed :c -> :h · forwarding :h -> :c
#
# Colors and glyphs mirror `ahjo top` (internal/tui/top/icons.go) so the two read
# as one tool. Live bridge state comes from ~/.ahjo/ahjo-state.json (the host
# rewrites it on every change). Host ports in the expose/forward segments are
# OSC 8 links to <scheme>://127.0.0.1:<port> (scheme guessed from the port).
set -o pipefail

REPO=/repo
STATE="$HOME/.ahjo/ahjo-state.json"

# git wrapper: always target /repo and trust it, so the dot reflects real state
# even when the statusline runs as a user that doesn't own the checkout (e.g.
# root) instead of tripping git's dubious-ownership guard.
g() { git -C "$REPO" -c safe.directory="$REPO" "$@"; }

# 24-bit ANSI, matching ahjo top's Primer dark-theme tokens.
GREEN=$'\033[38;2;63;185;80m'   # git clean
AMBER=$'\033[38;2;210;153;34m'  # git dirty / ahead / stashed
RED=$'\033[38;2;248;81;73m'     # git error
BLUE=$'\033[38;2;35;143;249m'   # mirror active
DIM=$'\033[38;5;245m'           # separators
RESET=$'\033[0m'

segments=()

# --- git dir dot: mirrors renderGitIcon — error, else dirty/ahead/stash, else clean.
# "dirty" is the union of anything not yet on the remote; behind-only is not surfaced.
git_dot() {
  local porcelain ahead stash
  if ! porcelain=$(g status --porcelain=v2 --branch 2>/dev/null); then
    printf '%s!%s' "$RED" "$RESET"
    return
  fi
  ahead=$(awk '/^# branch.ab/ {gsub(/\+/,"",$3); print $3}' <<<"$porcelain")
  stash=$(g stash list 2>/dev/null | wc -l | tr -d ' ')
  if grep -qv '^#' <<<"$porcelain" || [[ "${ahead:-0}" -gt 0 ]] || [[ "${stash:-0}" -gt 0 ]]; then
    printf '%s○%s' "$AMBER" "$RESET"
  else
    printf '%s●%s' "$GREEN" "$RESET"
  fi
}
segments+=("$(git_dot)")

# --- branch: prefer the live checkout, fall back to the state alias, then sha ---
branch=$(g branch --show-current 2>/dev/null)
[[ -z "$branch" ]] && branch=$(jq -r '.alias // empty' "$STATE" 2>/dev/null)
[[ -z "$branch" ]] && branch=$(g rev-parse --short HEAD 2>/dev/null)
[[ -n "$branch" ]] && segments+=("$branch")

# OSC 8 hyperlink (same scheme ahjo top uses): cmd/ctrl-click opens the target.
osc8() { printf '\033]8;;%s\033\\%s\033]8;;\033\\' "$1" "$2"; }
# Guess a URL scheme from a port: https for the canonical TLS ports, else http.
scheme() { case "$1" in 443 | 8443) echo https ;; *) echo http ;; esac; }
# Clickable host port: ":<port>" linking to <scheme>://127.0.0.1:<port>. Only the
# host side is linked — it resolves on the host, where the terminal (and the
# user's browser) live; the container port has no host-reachable address.
hostport() { osc8 "$(scheme "$1")://127.0.0.1:$1" ":$1"; }

# --- bridges: shown only when on, read from the live state file ---
if [[ -f "$STATE" ]]; then
  if [[ "$(jq -r '.mirror.on // false' "$STATE" 2>/dev/null)" == "true" ]]; then
    target=$(jq -r '.mirror.host_target // empty' "$STATE" 2>/dev/null)
    label="${BLUE}←${RESET} mirroring"
    # Link to the host-side mirror dir; the host_target is absolute, so
    # file://<path> already yields the three-slash file:///… form.
    if [[ -n "$target" ]]; then
      segments+=("$(osc8 "file://$target" "$label")")
    else
      segments+=("$label")
    fi
  fi

  expose=$(jq -r '.expose // [] | .[] | "\(.container) \(.host)"' "$STATE" 2>/dev/null)
  if [[ -n "$expose" ]]; then
    parts=()
    while read -r container host; do
      [[ -z "$host" ]] && continue
      parts+=(":$container -> $(hostport "$host")")
    done <<<"$expose"
    segments+=("exposed ${parts[*]}")
  fi

  forward=$(jq -r '.forward // [] | .[] | "\(.host) \(.container)"' "$STATE" 2>/dev/null)
  if [[ -n "$forward" ]]; then
    parts=()
    while read -r host container; do
      [[ -z "$host" ]] && continue
      parts+=("$(hostport "$host") -> :$container")
    done <<<"$forward"
    segments+=("forwarding ${parts[*]}")
  fi
fi

# --- join ---
out=""
sep=" ${DIM}·${RESET} "
for i in "${!segments[@]}"; do
  if [[ $i -eq 0 ]]; then out="${segments[$i]}"; else out="${out}${sep}${segments[$i]}"; fi
done
printf '%s' "$out"
