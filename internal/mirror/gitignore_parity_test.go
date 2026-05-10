package mirror_test

// gitignore_parity_test.go pins the daemon's gitignore matcher to
// `git check-ignore`'s verdict on a curated set of fixtures (A–F) plus
// the live ahjo repo. This catches a future library swap, an upstream
// regression, or a new repo convention silently breaking parity — the
// kind of failure mode that produces ghost files on the Mac without
// announcing itself.
//
// The fixtures are produced by research/spike-gitignore/make-fixtures.sh,
// invoked into a t.TempDir() per test run. The script needs git + bash
// on PATH; tests skip if either is missing (CI environments always have
// both, but a developer building offline shouldn't see a hard fail).
//
// We compare on FILE paths only. The single documented disagreement
// between go-git and `git check-ignore` is the C-globs fixture's `qux/`
// directory entry (go-git: ignored; git: not — git's "qux/** matches
// contents but not the directory itself" distinction). Files under qux/
// agree on every fixture, and the daemon only ever copies files, so the
// dir-entry difference is functionally invisible.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

const fixtureScript = "../../research/spike-gitignore/make-fixtures.sh"

func TestGitignoreParity_SpikeFixtures(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	tmp := t.TempDir()
	cmd := exec.Command("bash", fixtureScript, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixtures: %v\n%s", err, out)
	}

	fixtures := []string{"A-flat", "B-nested", "C-globs", "D-neg", "E-monorepo", "F-env"}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			assertFileParity(t, filepath.Join(tmp, name))
		})
	}
}

func TestGitignoreParity_AhjoRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skipf("not a git repo at %s", root)
	}
	assertFileParity(t, root)
}

// assertFileParity walks `root`, classifies every regular file with both
// our matcher and `git check-ignore`, and t.Errorf's on any disagreement.
// `noSkiplist=true` is intentional: we want to stress every gitignore rule,
// not let SkipDirNames mask divergences in node_modules-style trees.
func assertFileParity(t *testing.T, root string) {
	t.Helper()

	matcher, err := mirror.LoadIgnoreMatcher(root, true)
	if err != nil {
		t.Fatalf("load matcher: %v", err)
	}

	files := walkFiles(t, root)
	if len(files) == 0 {
		t.Skipf("no files under %s", root)
	}

	gitVerdict := classifyGit(t, root, files)

	disagree := 0
	var examples []string
	for _, rel := range files {
		ours := effectivelyIgnoredFile(matcher, rel)
		theirs, ok := gitVerdict[rel]
		if !ok {
			theirs = false
		}
		if ours != theirs {
			disagree++
			if len(examples) < 10 {
				examples = append(examples,
					fmt.Sprintf("  %s: ours=%v git=%v", rel, ours, theirs))
			}
		}
	}
	if disagree > 0 {
		t.Errorf("%d disagreement(s) of %d files in %s:\n%s",
			disagree, len(files), root, strings.Join(examples, "\n"))
	}
}

// effectivelyIgnoredFile returns true iff our matcher would skip this file
// from being mirrored — checking the file itself AND every ancestor dir.
// This mirrors what the daemon actually does: ignored ancestor dirs are
// SkipDir-ed during walk, so descendants never reach the copy routine.
func effectivelyIgnoredFile(m interface {
	Match(path []string, isDir bool) bool
}, rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i < len(parts); i++ {
		if m.Match(parts[:i], true) {
			return true
		}
	}
	return m.Match(parts, false)
}

// walkFiles returns every regular file under root as a sorted slice of
// repo-relative paths, skipping the .git directory.
func walkFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// classifyGit batches every relative path through `git check-ignore -v
// --non-matching --stdin` and parses the verbose output. Format:
//
//	<source>:<linenum>:<pattern>\t<pathname>      → matched
//	::\t<pathname>                                → not matched
//
// Patterns starting with `!` re-include via negation; we honor that.
func classifyGit(t *testing.T, root string, paths []string) map[string]bool {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "check-ignore",
		"--verbose", "--non-matching", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// `git check-ignore` exits 0 if any path matched, 1 if none matched, 128
	// on usage error. Treat 0 and 1 as success; anything else is a failure.
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() > 1 {
			t.Fatalf("git check-ignore: %v\nstderr: %s", err, stderr.String())
		}
	}
	verdict := map[string]bool{}
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		tab := strings.LastIndex(line, "\t")
		if tab < 0 {
			continue
		}
		path := line[tab+1:]
		prefix := line[:tab]
		ignored := !strings.HasPrefix(prefix, "::")
		if ignored {
			c1 := strings.Index(prefix, ":")
			if c1 >= 0 {
				if c2 := strings.Index(prefix[c1+1:], ":"); c2 >= 0 {
					pattern := prefix[c1+1+c2+1:]
					if strings.HasPrefix(pattern, "!") {
						ignored = false
					}
				}
			}
		}
		verdict[path] = ignored
	}
	return verdict
}
