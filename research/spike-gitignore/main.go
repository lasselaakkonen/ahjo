// Spike: three-way gitignore parity against git check-ignore.
//
// For each fixture, walk every path; for each path emit four verdicts:
//   - git check-ignore (ground truth)
//   - sabhiram/go-gitignore (single-file pattern set, root .gitignore only)
//   - go-git plumbing/format/gitignore (nested-aware)
//   - rsync --dry-run --filter=:- .gitignore (bootstrap path)
//
// Output: per-fixture disagreement counts + first 5 disagreements per pair.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gitignore "github.com/go-git/go-git/v5/plumbing/format/gitignore"
	sabhiram "github.com/sabhiram/go-gitignore"
)

// runFixture is the per-fixture entry point. fixDir is an existing tree
// (already populated, with .git initialised by the harness so check-ignore
// works); the fixture's full set of paths is walked and each is classified.
func runFixture(name, fixDir string) {
	fmt.Printf("\n========== fixture: %s ==========\n", name)
	fmt.Printf("dir: %s\n", fixDir)

	paths := walk(fixDir)
	fmt.Printf("paths walked: %d\n", len(paths))

	gitVerdict := classifyGit(fixDir, paths)
	sabVerdict := classifySabhiram(fixDir, paths)
	gogitVerdict := classifyGoGit(fixDir, paths)
	rsyncVerdict := classifyRsync(fixDir, paths)

	report("git vs sabhiram", paths, gitVerdict, sabVerdict)
	report("git vs go-git ", paths, gitVerdict, gogitVerdict)
	report("git vs rsync  ", paths, gitVerdict, rsyncVerdict)
	report("rsync vs go-git", paths, rsyncVerdict, gogitVerdict)
}

func walk(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Skip the .git dir for classification — we never mirror it and
		// git check-ignore rejects paths inside .git anyway.
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
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

// classifyGit invokes `git check-ignore -v --non-matching --no-index` for
// every path in one batch via stdin. Returns ignored=true iff git would
// exclude the path.
func classifyGit(root string, paths []string) map[string]bool {
	cmd := exec.Command("git", "-C", root, "check-ignore", "--verbose", "--non-matching", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() // exits 0 if any matched, 1 if none, 128 on error; we don't care
	verdict := map[string]bool{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "<source>:<linenum>:<pattern>\t<pathname>"
		// Non-matching: "::\t<pathname>"
		tab := strings.LastIndex(line, "\t")
		if tab < 0 {
			continue
		}
		path := line[tab+1:]
		prefix := line[:tab]
		// Non-matching: "::" prefix → not ignored.
		// Matching: "<source>:<linenum>:<pattern>" — re-includes via `!` mean
		// the path is NOT ignored even though git emits a matched line.
		ignored := !strings.HasPrefix(prefix, "::")
		if ignored {
			// Pattern is the substring after the second colon.
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
	for _, p := range paths {
		if _, ok := verdict[p]; !ok {
			verdict[p] = false // not matched = not ignored
		}
	}
	return verdict
}

// classifySabhiram loads only the ROOT .gitignore (the library's natural
// usage). This is the failure mode we want to surface: nested .gitignore
// files in subdirs are not consulted.
func classifySabhiram(root string, paths []string) map[string]bool {
	rootGI := filepath.Join(root, ".gitignore")
	verdict := map[string]bool{}
	gi, err := sabhiram.CompileIgnoreFile(rootGI)
	if err != nil {
		// No root gitignore — nothing ignored.
		for _, p := range paths {
			verdict[p] = false
		}
		return verdict
	}
	for _, p := range paths {
		verdict[p] = gi.MatchesPath(p)
	}
	return verdict
}

// classifyGoGit walks all .gitignore files in the tree and feeds them to
// gitignore.Matcher with their per-dir domains. This is what a nested-aware
// integration would look like in production.
func classifyGoGit(root string, paths []string) map[string]bool {
	var patterns []gitignore.Pattern
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(p) != ".gitignore" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		domain := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
		if len(domain) == 1 && domain[0] == "." {
			domain = nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			patterns = append(patterns, gitignore.ParsePattern(line, domain))
		}
		return nil
	})
	m := gitignore.NewMatcher(patterns)
	verdict := map[string]bool{}
	for _, p := range paths {
		parts := strings.Split(filepath.ToSlash(p), "/")
		// Determine isDir
		full := filepath.Join(root, p)
		st, err := os.Lstat(full)
		isDir := err == nil && st.IsDir()
		verdict[p] = m.Match(parts, isDir)
	}
	return verdict
}

// classifyRsync runs `rsync -an --filter=:- .gitignore --exclude=.git
// <root>/ /tmp/empty/` and reads the file list from --dry-run output.
// Anything in the source tree that does NOT appear is excluded.
func classifyRsync(root string, paths []string) map[string]bool {
	dst, err := os.MkdirTemp("", "rsync-dst-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dst)
	cmd := exec.Command("rsync",
		"-an", "--itemize-changes",
		"--filter=:- .gitignore", "--exclude=.git",
		root+"/", dst+"/",
	)
	out, err := cmd.CombinedOutput()
	_ = err
	included := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// rsync itemized: "<flags> <path>" — e.g. ">f+++++++++ src/main.js"
		// or "cd+++++++++ src/" for dirs
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		path := strings.TrimRight(fields[1], "/")
		included[path] = true
	}
	verdict := map[string]bool{}
	for _, p := range paths {
		// rsync EXCLUDES are paths NOT in the included set.
		// But rsync only emits dirs in its dry-run output; descendants
		// of an excluded dir don't appear at all. We need to check
		// whether the path or any ancestor is excluded.
		excluded := !included[p]
		if excluded {
			// If any ancestor is included, the path itself is excluded
			// individually; that's what we want to record.
			// (No special handling needed — `excluded` already holds.)
		}
		verdict[p] = excluded
	}
	return verdict
}

// report prints disagreements between two verdict maps.
func report(label string, paths []string, a, b map[string]bool) {
	disagree := 0
	var examples []string
	for _, p := range paths {
		if a[p] != b[p] {
			disagree++
			if len(examples) < 5 {
				examples = append(examples,
					fmt.Sprintf("    %s: %v vs %v", p, a[p], b[p]))
			}
		}
	}
	pct := 0.0
	if len(paths) > 0 {
		pct = 100.0 * float64(disagree) / float64(len(paths))
	}
	fmt.Printf("  %s: %d disagreements / %d paths (%.1f%%)\n",
		label, disagree, len(paths), pct)
	for _, e := range examples {
		fmt.Println(e)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike <fixture-dir> [<name>]")
		os.Exit(2)
	}
	dir := os.Args[1]
	name := dir
	if len(os.Args) >= 3 {
		name = os.Args[2]
	}
	runFixture(name, dir)
}
