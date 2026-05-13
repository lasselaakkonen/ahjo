#!/bin/sh
# ahjo paste shim for /usr/local/bin/wl-paste. Same role as the xclip shim:
# bridges Claude's image-paste probe to the macOS host pasteboard via the
# ahjo paste-daemon at 127.0.0.1:18340.
#
# Three invocations are honored:
#   - `wl-paste --list-types`           → list MIME types
#   - `wl-paste --type image/png`       → bytes
#   - `wl-paste --type image/jpeg`      → same bytes (always PNG)
# Non-image probes exit 1, matching wl-paste's "no selection" behavior.

LOG=/tmp/ahjo-paste-shim.log
printf '[%s] wl-paste pid=%d argv=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" "$*" >> "$LOG" 2>/dev/null

mime=""
listmode=0
while [ $# -gt 0 ]; do
	case "$1" in
		-t|--type)
			shift; mime="${1:-}"; shift ;;
		--type=*)
			mime="${1#*=}"; shift ;;
		-l|--list-types)
			listmode=1; shift ;;
		-p|--primary|-n|--no-newline|-w|--watch)
			shift ;;
		*)
			shift ;;
	esac
done

# --list-types: probe daemon, emit image/png only when one is available.
if [ "$listmode" = "1" ]; then
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
	printf '[%s] wl-paste miss status=%s mime=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$status" "$mime" >> "$LOG" 2>/dev/null
	exit 1
fi
printf '[%s] wl-paste ok bytes=%d mime=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(wc -c < "$tmp")" "$mime" >> "$LOG" 2>/dev/null
cat "$tmp"
rm -f "$tmp"
