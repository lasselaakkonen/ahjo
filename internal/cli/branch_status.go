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
		applyGitStatus(container, &bs)
	}

	owner, name, ok := parseGitHubRepo(remote)
	if ok && br.Branch != "" && container != "" {
		applyPRStatus(container, owner, name, br.Branch, &bs)
	}
	return bs, nil
}

// applyGitStatus runs `git status` inside the container and folds the
// result (or error) into bs. Extracted from fetchBranchStatusInVM so the
// post-attach renderer can call it directly from a goroutine.
func applyGitStatus(container string, bs *top.BranchStatus) {
	// -c safe.directory=/repo: incus exec runs as root, but /repo is
	// owned by the container's ubuntu user — without this override git
	// refuses with "fatal: detected dubious ownership". Scoped to /repo
	// rather than "*" so it doesn't loosen anything unrelated.
	out, err := runCapturing("incus", "exec", container, "--",
		"git", "-c", "safe.directory=/repo",
		"-C", "/repo", "status", "--porcelain=v1", "--branch")
	if err != nil {
		bs.GitErr = err.Error()
		return
	}
	parseGitStatus(out, bs)
	bs.GitChecked = true
}

// applyPRStatus runs `gh pr list` inside the container and folds the
// result (or error) into bs. gh runs in-container so the host doesn't
// need its own gh+auth setup. --state all surfaces merged/closed PRs;
// --limit 1 because gh sorts newest-first and we only want the most
// recent for this head. statusCheckRollup is summarized down to a single
// "passed/checking/failed" tag so the RPC payload stays small.
func applyPRStatus(container, owner, name, branch string, bs *top.BranchStatus) {
	out, err := runCapturing("incus", "exec", container, "--",
		"gh", "pr", "list",
		"-R", owner+"/"+name,
		"--head", branch,
		"--state", "all",
		"--limit", "1",
		"--json", "number,url,state,title,statusCheckRollup")
	if err != nil {
		bs.PRErr = err.Error()
		return
	}
	var rows []prRow
	if jerr := json.Unmarshal(out, &rows); jerr != nil {
		bs.PRErr = fmt.Errorf("parse gh pr list: %w", jerr).Error()
		return
	}
	if len(rows) > 0 {
		r := rows[0]
		pr := top.PRStatus{
			Number: r.Number,
			URL:    r.URL,
			State:  r.State,
			Title:  r.Title,
		}
		if strings.EqualFold(r.State, "OPEN") {
			pr.Checks = summarizeChecks(r.StatusCheckRollup)
		}
		bs.PR = &pr
	}
	bs.PRChecked = true
}

// prRow shadows top.PRStatus for unmarshalling the gh response so we can
// pull statusCheckRollup off the wire without bloating top.PRStatus with
// fields it never reads.
type prRow struct {
	Number            int          `json:"number"`
	URL               string       `json:"url"`
	State             string       `json:"state"`
	Title             string       `json:"title"`
	StatusCheckRollup []checkEntry `json:"statusCheckRollup"`
}

// checkEntry unions the two shapes gh returns inside statusCheckRollup:
// CheckRun (status+conclusion) and StatusContext (state). We don't get
// __typename from gh; presence of `status` is enough to disambiguate.
type checkEntry struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// summarizeChecks folds a rollup into one label. Priority: any failure
// wins, then any in-flight check, otherwise passed. Empty rollup → ""
// so the renderer knows to drop the comma-suffix entirely.
func summarizeChecks(rollup []checkEntry) string {
	if len(rollup) == 0 {
		return ""
	}
	hasPending := false
	for _, c := range rollup {
		switch classifyCheck(c) {
		case "failed":
			return "failed"
		case "pending":
			hasPending = true
		}
	}
	if hasPending {
		return "checking"
	}
	return "passed"
}

// classifyCheck reduces one rollup entry to "failed", "pending", or "ok".
// CheckRun: not-yet-COMPLETED is pending; a COMPLETED run's conclusion
// decides. StatusContext: state alone decides. Conclusions/states we
// don't recognise (NEUTRAL, SKIPPED, STALE…) fall through as "ok" — they
// shouldn't block a "passed" rollup.
func classifyCheck(c checkEntry) string {
	if c.Status != "" {
		if !strings.EqualFold(c.Status, "COMPLETED") {
			return "pending"
		}
		switch strings.ToUpper(c.Conclusion) {
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
			return "failed"
		}
		return "ok"
	}
	switch strings.ToUpper(c.State) {
	case "FAILURE", "ERROR":
		return "failed"
	case "PENDING", "EXPECTED":
		return "pending"
	}
	return "ok"
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
