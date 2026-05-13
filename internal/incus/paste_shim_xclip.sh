#!/bin/sh
# ahjo paste shim for /usr/local/bin/xclip. Bridges Claude Code's
# image-paste probe to the macOS host pasteboard via the ahjo paste-daemon
# reached at 127.0.0.1:18340 (Incus proxy device -> host.lima.internal).
#
# Three invocations are honored:
#   - `xclip -selection clipboard -t TARGETS -o`     → list MIME types
#   - `xclip -selection clipboard -t image/png -o`   → bytes
#   - `xclip -selection clipboard -t image/jpeg -o`  → same bytes (always PNG)
# Anything else exits 1, matching real xclip's "no selection" behavior so
# Claude falls through silently.
#
# Diagnostic logging: every invocation appends to /tmp/ahjo-paste-shim.log
# so the user can `tail` it after a `Ctrl+V` to see exactly what Claude
# asked for. Stays under /tmp to avoid escaping into git via /repo.

LOG=/tmp/ahjo-paste-shim.log
printf '[%s] xclip pid=%d argv=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" "$*" >> "$LOG" 2>/dev/null

mime=""
mode="out"
while [ $# -gt 0 ]; do
	case "$1" in
		-t|--type|-target|--target)
			shift; mime="${1:-}"; shift ;;
		--type=*|--target=*)
			mime="${1#*=}"; shift ;;
		-selection|--selection|-display|--display|-d|-loops|--loops|-l)
			shift; [ $# -gt 0 ] && shift ;;
		-o|--out|-out)
			mode="out"; shift ;;
		-i|--in|-in|-f|--filter|-filter)
			mode="in"; shift ;;
		*)
			shift ;;
	esac
done

[ "$mode" = "out" ] || exit 1

# TARGETS query: list the MIME types we can supply. Real xclip emits the
# full /usr/include/X11/Xatom.h zoo here (TIMESTAMP, MULTIPLE, …); Claude
# only needs to find "image/png" in the output, so emitting just that is
# both sufficient and unambiguous about what's available.
if [ "$mime" = "TARGETS" ]; then
	# Probe daemon: only claim the target exists if the daemon currently
	# serves it, so claude doesn't pre-commit to an image fetch that's
	# guaranteed to come back empty.
	status=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 2 \
		"http://127.0.0.1:18340/image.png" 2>/dev/null)
	if [ "$status" = "200" ]; then
		printf 'image/png\n'
		exit 0
	fi
	exit 1
fi

case "$mime" in
	image/*) ;;
	*) exit 1 ;;
esac

tmp=$(mktemp 2>/dev/null) || exit 1
status=$(curl -sS -o "$tmp" -w "%{http_code}" --max-time 5 \
	"http://127.0.0.1:18340/image.png" 2>/dev/null)
if [ "$status" != "200" ]; then
	rm -f "$tmp"
	printf '[%s] xclip miss status=%s mime=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$status" "$mime" >> "$LOG" 2>/dev/null
	exit 1
fi
printf '[%s] xclip ok bytes=%d mime=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(wc -c < "$tmp")" "$mime" >> "$LOG" 2>/dev/null
cat "$tmp"
rm -f "$tmp"
