# gitignore parity spike

Run on 2026-05-10 to resolve Open Question 2 of `designdocs/in-container-mirror.md`.
Compares three implementations of `.gitignore` against `git check-ignore`:

- `github.com/sabhiram/go-gitignore` (single-file pattern set)
- `github.com/go-git/go-git/v5/plumbing/format/gitignore` (nested-aware)
- `rsync --filter=:- .gitignore` (rsync's per-dir merge)

## Reproduce

Inside an environment with go ≥1.23, git, and **GNU rsync** (the spike was
authored against rsync 3.2.7 in the ahjo Lima VM; macOS's openrsync was not
used):

```sh
go build -o spike ./main.go
bash make-fixtures.sh ./fixtures
for f in fixtures/*; do ./spike "$f" "$(basename $f)"; done
# Optional: snapshot the live ahjo repo and run against it
rsync -a --quiet /path/to/ahjo/ /tmp/ahjo-snapshot/
./spike /tmp/ahjo-snapshot ahjo-real-repo
```

## Findings

See `designdocs/in-container-mirror.md`'s "Spike: gitignore parity" section
for the result table and the design decision the spike forced (drop rsync
from the daemon entirely; live + bootstrap both use the go-git matcher).

The fixtures in `make-fixtures.sh` are the source for
`internal/mirror/gitignore_parity_test.go` (regression coverage that fires
if a future library swap or upstream change re-introduces a non-git-faithful
filter).
