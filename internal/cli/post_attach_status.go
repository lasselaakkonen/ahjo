package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// Overall wall-clock budget for the post-exit status block. Both fetches
// run in parallel; whichever hasn't returned by the deadline gets a
// "timed out" marker so the user still sees the side that did finish.
const postAttachDeadline = 6 * time.Second

var (
	postExitTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("#238FF9")).Bold(true)
	postExitLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	postExitValue = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	postExitDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// showPostAttachStatus prints a small status block summarizing the branch
// the user just exited: the alias + container name, then the same
// git-status and PR-status lines `ahjo top` shows in its detail pane.
//
// Skips silently when stdout isn't a TTY so piped/scripted runs of
// `ahjo shell` / `ahjo claude` produce clean output.
func showPostAttachStatus(br *registry.Branch, containerName string) {
	if br == nil || !isTerminal(os.Stdout) {
		return
	}

	alias := br.Slug
	if len(br.Aliases) > 0 {
		alias = br.Aliases[0]
	}

	remote := lookupRepoRemote(br.Repo)
	owner, name, hasGitHub := parseGitHubRepo(remote)
	wantPR := hasGitHub && br.Branch != ""

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, postExitTitle.Render(fmt.Sprintf("exited %s", alias))+
		" "+postExitDim.Render(fmt.Sprintf("(container %s)", containerName)))

	bs := fetchPostAttachStatus(containerName, owner, name, br.Branch, wantPR)

	printPostAttachRow("git", top.FormatGitStatus(&bs))
	if wantPR {
		printPostAttachRow("pr", top.FormatPRStatus(&bs))
	}

	if shouldOfferCleanup(&bs) {
		offerCleanupAfterMerge(br, containerName, &bs)
	}
}

// shouldOfferCleanup gates the post-exit cleanup prompts on /repo being
// fully clean (no working-tree changes, no unpushed commits) AND the PR
// having reached MERGED. Both halves must have been fetched successfully —
// a timeout or error on either side suppresses the offer so we never
// prompt off a half-known state.
func shouldOfferCleanup(bs *top.BranchStatus) bool {
	if !bs.GitChecked || bs.GitErr != "" {
		return false
	}
	if bs.Dirty || bs.Ahead > 0 {
		return false
	}
	if !bs.PRChecked || bs.PRErr != "" || bs.PR == nil {
		return false
	}
	return strings.EqualFold(bs.PR.State, "MERGED")
}

// offerCleanupAfterMerge asks the two follow-up questions in the order
// the user expects to see them (container first, then remote branch),
// then executes in the order they have to run: the remote-branch delete
// goes through `git push` inside the container, so it must happen
// before the container is torn down.
func offerCleanupAfterMerge(br *registry.Branch, containerName string, bs *top.BranchStatus) {
	fmt.Fprintln(os.Stdout)

	removeContainer := promptYesNo(fmt.Sprintf("PR is merged and /repo is clean — remove container %s?", containerName))

	removeRemote := false
	if br.Branch != "" {
		removeRemote = promptYesNo(fmt.Sprintf("Delete remote branch origin/%s?", br.Branch))
	}

	if removeRemote {
		if err := deleteRemoteBranch(containerName, br.Branch); err != nil {
			fmt.Fprintf(os.Stderr, "warn: delete remote branch: %v\n", err)
		} else {
			fmt.Fprintf(os.Stdout, "deleted origin/%s\n", br.Branch)
		}
	}

	if removeContainer {
		alias := br.Slug
		if len(br.Aliases) > 0 {
			alias = br.Aliases[0]
		}
		if err := runRm(alias, false, false); err != nil {
			fmt.Fprintf(os.Stderr, "warn: remove container: %v\n", err)
		}
	}
}

// deleteRemoteBranch runs `git push origin --delete <branch>` inside the
// container so it picks up the in-container credential helper (gh's
// stored PAT) rather than needing host-side auth. safe.directory mirrors
// what applyGitStatus uses — incus exec runs as root over a /repo owned
// by ubuntu.
func deleteRemoteBranch(container, branch string) error {
	_, err := incus.Exec(container, "git", "-c", "safe.directory=/repo",
		"-C", "/repo", "push", "origin", "--delete", branch)
	return err
}

// fetchPostAttachStatus runs the git-status and (optionally) PR-status
// fetches concurrently, animates a spinner on stderr while they run, and
// returns once both have finished or the deadline fires. Orphan
// goroutines after timeout are fine — the ahjo process is about to exit.
func fetchPostAttachStatus(container, owner, name, branch string, wantPR bool) top.BranchStatus {
	bs := top.BranchStatus{FetchedAt: time.Now()}
	var mu sync.Mutex

	gitDone := make(chan struct{})
	go func() {
		var local top.BranchStatus
		applyGitStatus(container, &local)
		mu.Lock()
		bs.GitChecked = local.GitChecked
		bs.GitErr = local.GitErr
		bs.Dirty = local.Dirty
		bs.DirtyFiles = local.DirtyFiles
		bs.HeadBranch = local.HeadBranch
		bs.Ahead = local.Ahead
		bs.Behind = local.Behind
		mu.Unlock()
		close(gitDone)
	}()

	var prDone chan struct{}
	if wantPR {
		prDone = make(chan struct{})
		go func() {
			var local top.BranchStatus
			applyPRStatus(container, owner, name, branch, &local)
			mu.Lock()
			bs.PRChecked = local.PRChecked
			bs.PRErr = local.PRErr
			bs.PR = local.PR
			mu.Unlock()
			close(prDone)
		}()
	}

	spinStop := make(chan struct{})
	spinDone := make(chan struct{})
	go runSpinner(spinStop, spinDone)

	deadline := time.After(postAttachDeadline)
	gitPending := true
	prPending := wantPR
	for gitPending || prPending {
		select {
		case <-gitDone:
			gitPending = false
			gitDone = nil
		case <-prDone:
			prPending = false
			prDone = nil
		case <-deadline:
			mu.Lock()
			if gitPending && bs.GitErr == "" {
				bs.GitErr = "timed out"
			}
			if prPending && bs.PRErr == "" {
				bs.PRErr = "timed out"
			}
			mu.Unlock()
			gitPending = false
			prPending = false
		}
	}

	close(spinStop)
	<-spinDone
	mu.Lock()
	out := bs
	mu.Unlock()
	return out
}

// runSpinner draws a single-line spinner on stderr until stop is closed,
// then erases it. stderr (not stdout) so the spinner doesn't contaminate
// any caller that redirects stdout, even though we already gated the
// whole renderer on stdout-is-a-TTY.
func runSpinner(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	i := 0
	render := func() {
		fmt.Fprintf(os.Stderr, "\r%s checking branch status… ", frames[i%len(frames)])
	}
	render()
	for {
		select {
		case <-stop:
			fmt.Fprint(os.Stderr, "\r\033[K")
			return
		case <-tick.C:
			i++
			render()
		}
	}
}

func printPostAttachRow(label, value string) {
	fmt.Fprintln(os.Stdout,
		postExitLabel.Render(fmt.Sprintf("%-6s", label+":"))+
			" "+postExitValue.Render(value))
}

// lookupRepoRemote returns the configured git remote URL for the repo
// row matching name, or "" when the registry can't be loaded or the row
// is missing. Used only to decide whether to attempt the PR fetch and to
// derive owner/name for `gh pr list`.
func lookupRepoRemote(repoName string) string {
	reg, err := registry.Load()
	if err != nil {
		return ""
	}
	for i := range reg.Repos {
		if reg.Repos[i].Name == repoName {
			return reg.Repos[i].Remote
		}
	}
	return ""
}
