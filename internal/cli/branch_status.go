package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// newBranchStatusCmd is the hidden `ahjo branch-status <slug>` RPC endpoint
// the Mac shim invokes via `limactl shell ahjo`. It mirrors what the in-VM
// `ahjo top` does inline through Deps.LoadBranchStatus, but encodes the
// result as JSON so the Mac side can unmarshal it.
func newBranchStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "branch-status <slug>",
		Short:  "internal: emit branch status as JSON for the Mac TUI",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			bs, err := fetchBranchStatusInVM(args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(bs)
		},
	}
}

// newTopStateCmd is the hidden `ahjo top-state` RPC endpoint. Same pattern
// as branch-status: emit a top.Snapshot as JSON.
func newTopStateCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "top-state",
		Short:  "internal: emit `ahjo top` snapshot as JSON for the Mac TUI",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			snap, err := loadSnapshotInVM()
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(snap)
		},
	}
}

// fetchBranchStatusInVM resolves the branch by slug and runs the same two
// subprocesses the TUI used to run inline: `git status` inside the
// container, plus `gh pr list` when the remote is a github.com URL. Both
// are short-lived but hit external systems — callers should run this off
// the UI goroutine via a tea.Cmd.
func fetchBranchStatusInVM(slug string) (top.BranchStatus, error) {
	bs := top.BranchStatus{FetchedAt: time.Now()}
	reg, err := registry.Load()
	if err != nil {
		return bs, err
	}
	var br *registry.Branch
	for i := range reg.Branches {
		if reg.Branches[i].Slug == slug {
			br = &reg.Branches[i]
			break
		}
	}
	if br == nil {
		return bs, fmt.Errorf("no branch with slug %q", slug)
	}
	container, err := resolveContainerName(br)
	if err != nil {
		return bs, err
	}
	remote := ""
	for i := range reg.Repos {
		if reg.Repos[i].Name == br.Repo {
			remote = reg.Repos[i].Remote
			break
		}
	}

	if container != "" {
		// -c safe.directory=/repo: incus exec runs as root, but /repo is
		// owned by the container's ubuntu user — without this override git
		// refuses with "fatal: detected dubious ownership". Scoped to /repo
		// rather than "*" so it doesn't loosen anything unrelated.
		out, err := runCapturing("incus", "exec", container, "--",
			"git", "-c", "safe.directory=/repo",
			"-C", "/repo", "status", "--porcelain=v1", "--branch")
		if err != nil {
			bs.GitErr = err.Error()
		} else {
			parseGitStatus(out, &bs)
			bs.GitChecked = true
		}
	}

	owner, name, ok := parseGitHubRepo(remote)
	if ok && br.Branch != "" && container != "" {
		// gh runs inside the container so the host doesn't need its own
		// gh+auth setup — the container's devcontainer feature already
		// provides both. --state all surfaces merged/closed PRs too;
		// --limit 1 because gh sorts newest-first and we only want the most
		// recent for this head.
		out, err := runCapturing("incus", "exec", container, "--",
			"gh", "pr", "list",
			"-R", owner+"/"+name,
			"--head", br.Branch,
			"--state", "all",
			"--limit", "1",
			"--json", "number,url,state,title")
		if err != nil {
			bs.PRErr = err.Error()
		} else {
			var rows []top.PRStatus
			if jerr := json.Unmarshal(out, &rows); jerr != nil {
				bs.PRErr = fmt.Errorf("parse gh pr list: %w", jerr).Error()
			} else {
				if len(rows) > 0 {
					pr := rows[0]
					bs.PR = &pr
				}
				bs.PRChecked = true
			}
		}
	}
	return bs, nil
}

// runCapturing wraps exec.Command so we keep stdout for parsing AND attach
// stderr to the returned error. On failure we return an error whose Error()
// is the last non-empty stderr line, falling back to the original exec
// error when stderr is empty.
func runCapturing(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := lastErrLine(stderr.Bytes()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// lastErrLine returns the last non-blank line of stderr with common noise
// prefixes trimmed so the message reads cleanly when shown in a one-row
// detail field.
func lastErrLine(b []byte) string {
	for _, line := range reverseSplit(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, p := range []string{"Error: ", "error: ", "fatal: "} {
			line = strings.TrimPrefix(line, p)
		}
		return line
	}
	return ""
}

func reverseSplit(s, sep string) []string {
	parts := strings.Split(s, sep)
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

// parseGitStatus consumes `git status --porcelain=v1 --branch` output. The
// first "## branch...upstream [ahead N, behind M]" line carries tracking
// info; every subsequent non-blank line is one modified entry.
func parseGitStatus(out []byte, bs *top.BranchStatus) {
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			parseBranchHeader(line[3:], bs)
			continue
		}
		bs.Dirty = true
		bs.DirtyFiles++
	}
}

func parseBranchHeader(rest string, bs *top.BranchStatus) {
	parts := strings.SplitN(rest, "...", 2)
	bs.HeadBranch = parts[0]
	if len(parts) != 2 {
		return
	}
	bracket := parts[1]
	i := strings.Index(bracket, "[")
	if i < 0 {
		return
	}
	track := strings.TrimSuffix(bracket[i+1:], "]")
	for _, p := range strings.Split(track, ",") {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "ahead "):
			n, _ := strconv.Atoi(strings.TrimPrefix(p, "ahead "))
			bs.Ahead = n
		case strings.HasPrefix(p, "behind "):
			n, _ := strconv.Atoi(strings.TrimPrefix(p, "behind "))
			bs.Behind = n
		}
	}
}

// parseGitHubRepo extracts owner/name from common GitHub remote URL shapes:
// `git@github.com:o/n[.git]`, `ssh://git@github.com/o/n[.git]`, and
// `https://github.com/o/n[.git]`. Returns ok=false for non-github hosts so
// the caller silently skips the PR fetch.
var (
	sshRemoteRE  = regexp.MustCompile(`^(?:git@|ssh://(?:[^@]+@)?)([^:/]+)[:/]([^/]+)/(.+?)/?$`)
	httpRemoteRE = regexp.MustCompile(`^https?://(?:[^@/]+@)?([^/]+)/([^/]+)/(.+?)/?$`)
)

func parseGitHubRepo(remote string) (owner, name string, ok bool) {
	remote = strings.TrimSpace(remote)
	for _, re := range []*regexp.Regexp{sshRemoteRE, httpRemoteRE} {
		m := re.FindStringSubmatch(remote)
		if m == nil {
			continue
		}
		host, o, n := m[1], m[2], strings.TrimSuffix(m[3], ".git")
		if !strings.EqualFold(host, "github.com") {
			return "", "", false
		}
		if o == "" || n == "" {
			return "", "", false
		}
		return o, n, true
	}
	return "", "", false
}
