#!/usr/bin/env bash
# Build five gitignore-parity fixtures under $1 (default ./fixtures).
#
# Each fixture is a self-contained git repo with a populated tree and one
# or more .gitignore files exercising a specific corner of the spec.
set -euo pipefail
ROOT="${1:-./fixtures}"
rm -rf "$ROOT"
mkdir -p "$ROOT"

mk_repo() {
    local dir="$1"
    mkdir -p "$dir"
    (cd "$dir" && git init -q -b main && \
     git config user.email s@e && git config user.name s)
}

# ---- A: flat ---- single root .gitignore, common patterns + one negation
A="$ROOT/A-flat"
mk_repo "$A"
cat >"$A/.gitignore" <<'EOF'
*.log
dist/
build/
node_modules/
*.tmp
!important.log
EOF
mkdir -p "$A/src" "$A/dist" "$A/build" "$A/node_modules/lib"
echo x > "$A/app.log"
echo x > "$A/important.log"
echo x > "$A/src/main.js"
echo x > "$A/src/main.log"
echo x > "$A/dist/output.txt"
echo x > "$A/build/artifact.bin"
echo x > "$A/node_modules/lib/index.js"
echo x > "$A/temp.tmp"
echo x > "$A/keep.txt"

# ---- B: nested ---- per-dir .gitignore in subdirs
B="$ROOT/B-nested"
mk_repo "$B"
echo "*.log" > "$B/.gitignore"
mkdir -p "$B/src/sub" "$B/pkg"
echo "*.tmp" > "$B/src/.gitignore"
cat >"$B/src/sub/.gitignore" <<'EOF'
!keep.tmp
*.bak
EOF
echo "node_modules/" > "$B/pkg/.gitignore"
mkdir -p "$B/pkg/node_modules/x"
echo x > "$B/pkg/node_modules/x/y.js"
echo x > "$B/root.log"
echo x > "$B/keep.txt"
echo x > "$B/src/main.js"
echo x > "$B/src/file.tmp"
echo x > "$B/src/main.log"
echo x > "$B/src/sub/file.tmp"
echo x > "$B/src/sub/keep.tmp"
echo x > "$B/src/sub/old.bak"
echo x > "$B/pkg/lib.go"

# ---- C: globs ---- ** / leading slash / unanchored
C="$ROOT/C-globs"
mk_repo "$C"
cat >"$C/.gitignore" <<'EOF'
**/foo.txt
/bar.txt
baz.txt
qux/**
EOF
mkdir -p "$C/a/b" "$C/qux/sub"
echo x > "$C/foo.txt"          # matched by **/foo.txt
echo x > "$C/a/foo.txt"        # matched by **/foo.txt
echo x > "$C/a/b/foo.txt"      # matched by **/foo.txt
echo x > "$C/bar.txt"          # matched by /bar.txt (root only)
echo x > "$C/a/bar.txt"        # NOT matched (anchored)
echo x > "$C/baz.txt"          # matched by baz.txt (unanchored)
echo x > "$C/a/baz.txt"        # matched by baz.txt (unanchored, anywhere)
echo x > "$C/qux/inside.txt"   # matched by qux/**
echo x > "$C/qux/sub/deep.txt" # matched by qux/**
echo x > "$C/keep.txt"

# ---- D: negation chains ----
D="$ROOT/D-neg"
mk_repo "$D"
cat >"$D/.gitignore" <<'EOF'
*.log
!debug.log
debug-*.log
EOF
echo x > "$D/app.log"        # ignored
echo x > "$D/debug.log"      # NOT ignored (negated)
echo x > "$D/debug-1.log"    # ignored again (later rule re-includes pattern)
echo x > "$D/keep.txt"

# ---- E: realistic monorepo ----
E="$ROOT/E-monorepo"
mk_repo "$E"
cat >"$E/.gitignore" <<'EOF'
node_modules/
dist/
*.log
.env
coverage/
EOF
mkdir -p "$E/packages/api/src" "$E/packages/web/src" "$E/packages/web/.next"
cat >"$E/packages/web/.gitignore" <<'EOF'
.next/
out/
EOF
cat >"$E/packages/api/.gitignore" <<'EOF'
*.tmp
__pycache__/
EOF
mkdir -p "$E/packages/api/__pycache__"
echo x > "$E/packages/api/__pycache__/m.pyc"
echo x > "$E/packages/api/src/server.ts"
echo x > "$E/packages/api/scratch.tmp"
echo x > "$E/packages/web/.next/build.js"
echo x > "$E/packages/web/src/page.tsx"
echo x > "$E/.env"
echo x > "$E/README.md"
echo x > "$E/server.log"

# ---- F: env-like ---- canonical Vite/Next/Rails .env*+!.env.example
F="$ROOT/F-env"
mk_repo "$F"
cat >"$F/.gitignore" <<'EOF'
.env*
!.env.example
node_modules/
EOF
echo x > "$F/.env"
echo x > "$F/.env.local"
echo x > "$F/.env.example"
echo x > "$F/.env.production.local"
echo x > "$F/keep.txt"

echo "fixtures built under $ROOT" >&2
ls "$ROOT"
