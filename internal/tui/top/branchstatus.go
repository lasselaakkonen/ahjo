package top

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// branchStatus captures the per-branch git working-tree state and any
// associated GitHub PR. Both halves are fetched together by fetchBranchStatus
// and surfaced in the right-pane branch detail.
type branchStatus struct {
	// Git side — populated when `incus exec ... git status` succeeded.
	GitChecked bool
	Dirty      bool
	DirtyFiles int
	Ahead      int
	Behind     int
	HeadBranch string
	GitErr     error

	// PR side — populated when the remote is a github.com URL and `gh pr
	// list` returned cleanly. A nil PR with PRChecked=true means "no PR
	// for this head branch".
	PRChecked bool
	PR        *prStatus
	PRErr     error

	FetchedAt time.Time
}

type prStatus struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Title  string `json:"title"`
}

// fetchBranchStatus runs the two subprocesses sequentially. Both are
// short-lived (~tens of ms for git status, ~hundreds of ms for gh) but they
// hit external systems, so callers should treat this as best-run-async via
// a tea.Cmd. The container's checkout lives at /repo by convention.
func fetchBranchStatus(container, remote, branchRef string) branchStatus {
	bs := branchStatus{FetchedAt: time.Now()}

	if container != "" {
		out, err := exec.Command("incus", "exec", container, "--", "git", "-C", "/repo", "status", "--porcelain=v1", "--branch").Output()
		if err != nil {
			bs.GitErr = err
		} else {
			parseGitStatus(out, &bs)
			bs.GitChecked = true
		}
	}

	owner, name, ok := parseGitHubRepo(remote)
	if ok && branchRef != "" {
		// --state all so we surface merged/closed PRs too; --limit 1 because
		// gh sorts newest-first and we only want the most recent for this head.
		out, err := exec.Command("gh", "pr", "list",
			"-R", owner+"/"+name,
			"--head", branchRef,
			"--state", "all",
			"--limit", "1",
			"--json", "number,url,state,title").Output()
		if err != nil {
			bs.PRErr = err
		} else {
			var rows []prStatus
			if err := json.Unmarshal(out, &rows); err != nil {
				bs.PRErr = fmt.Errorf("parse gh pr list: %w", err)
			} else {
				if len(rows) > 0 {
					pr := rows[0]
					bs.PR = &pr
				}
				bs.PRChecked = true
			}
		}
	}
	return bs
}

// parseGitStatus consumes `git status --porcelain=v1 --branch` output. The
// first "## branch...upstream [ahead N, behind M]" line carries tracking
// info; every subsequent non-blank line is one modified entry.
func parseGitStatus(out []byte, bs *branchStatus) {
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

func parseBranchHeader(rest string, bs *branchStatus) {
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
