// Package preflight runs read-only environmental checks for `ahjo doctor`.
// Each check returns a Problem with a fix hint; ahjo never auto-installs.
package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

type Severity int

const (
	OK Severity = iota
	Warn
	Fail
)

type Problem struct {
	Severity Severity
	Title    string
	Detail   string
	Fix      string // exact command, or empty if non-actionable
}

// Run executes the standard host-side checks. Mac-specific checks live in
// internal/lima and are run by ahjo-mac.
func Run() []Problem {
	var ps []Problem
	ps = append(ps, checkBinary("incus", "sudo apt install incus"))
	ps = append(ps, checkBinary("git", "sudo apt install git"))
	ps = append(ps, checkBinary("ssh-keygen", "sudo apt install openssh-client"))
	ps = append(ps, checkAhjoDir())
	ps = append(ps, checkOAuthToken())
	ps = append(ps, checkAnyGHToken())
	ps = append(ps, checkRepoTokenForwarding()...)
	if p, ok := checkLegacyRepoEnv(); ok {
		ps = append(ps, p)
	}
	ps = append(ps, checkGitIdentity())
	ps = append(ps, checkStoragePool())
	ps = append(ps, checkAhjoBase())
	return ps
}

func checkBinary(name, fix string) Problem {
	if _, err := exec.LookPath(name); err != nil {
		return Problem{Severity: Fail, Title: name + " not on PATH", Fix: fix}
	}
	return Problem{Severity: OK, Title: name + " on PATH"}
}

func checkAhjoDir() Problem {
	d := paths.AhjoDir()
	st, err := os.Stat(d)
	if os.IsNotExist(err) {
		return Problem{
			Severity: Warn,
			Title:    paths.AhjoDirName + " missing",
			Detail:   d + " does not exist",
			Fix:      "ahjo doctor --init",
		}
	}
	if err != nil {
		return Problem{Severity: Fail, Title: "stat " + d + " failed", Detail: err.Error()}
	}
	if !st.IsDir() {
		return Problem{Severity: Fail, Title: d + " is not a directory"}
	}
	tmp, err := os.CreateTemp(d, ".doctor.tmp.*")
	if err != nil {
		return Problem{Severity: Fail, Title: d + " not writable", Detail: err.Error()}
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return Problem{Severity: OK, Title: d + " writable"}
}

func checkOAuthToken() Problem {
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return Problem{
			Severity: Fail,
			Title:    "CLAUDE_CODE_OAUTH_TOKEN not set",
			Fix:      "ahjo init  # runs claude setup-token and saves the token to ~/.ahjo/.env",
		}
	}
	return Problem{Severity: OK, Title: "CLAUDE_CODE_OAUTH_TOKEN set"}
}

// checkAnyGHToken surveys per-repo PATs (~/.ahjo-shared/repo-env/<slug>.env)
// and the global GH_TOKEN. A warn fires when no registered repo has a
// per-repo PAT *and* no global GH_TOKEN is set, since that means `gh` won't
// work in any container. Repos without their own PAT are listed so the user
// can decide which ones to set.
func checkAnyGHToken() Problem {
	reg, err := registry.Load()
	if err != nil {
		return Problem{Severity: Warn, Title: "could not load registry for GH_TOKEN survey", Detail: err.Error()}
	}
	globalSet := false
	if v, ok, err := tokenstore.Get(tokenstore.GHTokenEnv); err == nil && ok && v != "" {
		globalSet = true
	} else if os.Getenv(tokenstore.GHTokenEnv) != "" {
		globalSet = true
	}
	var withPAT, withoutPAT []string
	for i := range reg.Repos {
		slug := reg.Repos[i].Name
		_, found, _ := tokenstore.GetAt(paths.SlugEnvPath(slug), tokenstore.GHTokenEnv)
		if found {
			withPAT = append(withPAT, slug)
		} else {
			withoutPAT = append(withoutPAT, slug)
		}
	}
	if len(reg.Repos) == 0 {
		if globalSet {
			return Problem{Severity: OK, Title: "GH_TOKEN set globally (no repos registered)"}
		}
		return Problem{Severity: OK, Title: "no repos registered; GH_TOKEN survey skipped"}
	}
	if len(withPAT) > 0 && len(withoutPAT) == 0 {
		return Problem{Severity: OK, Title: fmt.Sprintf("per-repo GH_TOKEN set for all %d repo(s)", len(withPAT))}
	}
	if globalSet && len(withoutPAT) > 0 {
		return Problem{
			Severity: Warn,
			Title:    "global GH_TOKEN covers repos without their own PAT — broad scope",
			Detail:   fmt.Sprintf("no per-repo PAT for: %s", strings.Join(withoutPAT, ", ")),
			Fix:      "ahjo repo set-token <alias>  # fine-grained PAT scoped to one repo",
		}
	}
	if !globalSet && len(withPAT) > 0 {
		return Problem{
			Severity: Warn,
			Title:    "some repos have no GH_TOKEN and no global fallback",
			Detail:   fmt.Sprintf("missing for: %s", strings.Join(withoutPAT, ", ")),
			Fix:      "ahjo repo set-token <alias>",
		}
	}
	return Problem{
		Severity: Warn,
		Title:    "no GH_TOKEN set (per-repo or global); `gh` inside containers will be unauthenticated",
		Detail:   fmt.Sprintf("registered repos: %s", strings.Join(withoutPAT, ", ")),
		Fix:      "ahjo repo set-token <alias>  # or `ahjo env set GH_TOKEN \"$(gh auth token)\"` (broad)",
	}
}

// checkLegacyRepoEnv warns when per-repo .env files still exist at the
// previous location ~/.ahjo/repo-env/. The new location is
// ~/.ahjo-shared/repo-env/ so PATs live on the user's host filesystem (Mac
// home via virtiofs, Linux home on bare-metal) rather than inside the Lima
// VM disk image. Per the project's no-runtime-migration rule, the user
// recreates per-repo PATs (`ahjo repo set-token <alias>`) and then deletes
// the old dir themselves. Returns (Problem, true) when leftover .env files
// exist; (zero, false) otherwise so the caller can skip a noisy line.
func checkLegacyRepoEnv() (Problem, bool) {
	legacy := filepath.Join(paths.AhjoDir(), "repo-env")
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return Problem{}, false
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		slugs = append(slugs, strings.TrimSuffix(e.Name(), ".env"))
	}
	if len(slugs) == 0 {
		return Problem{}, false
	}
	return Problem{
		Severity: Warn,
		Title:    fmt.Sprintf("%d per-repo PAT file(s) at the legacy path %s", len(slugs), legacy),
		Detail: "ahjo no longer reads PATs from this directory. They moved to ~/.ahjo-shared/repo-env/ " +
			"so they live on the user's actual host filesystem (Mac home via virtiofs, or the user's home " +
			"on standalone Linux) instead of inside the Lima VM disk. Affected repos: " + strings.Join(slugs, ", "),
		Fix: "for each: ahjo repo set-token <alias>  # re-populates at the new path\n" +
			"       then: rm -rf " + legacy,
	}, true
}

// checkRepoTokenForwarding probes each registered repo's default-branch
// container for the two pieces of in-container plumbing that make raw
// `git clone/fetch/push/pull` over HTTPS work without per-call env juggling:
//
//  1. environment.GH_TOKEN set on the container (so every `incus exec`
//     inherits it, not just attach-time helpers).
//  2. credential.https://github.com.helper configured in the in-container
//     /home/ubuntu/.gitconfig (written by `gh auth setup-git`).
//
// Skips repos whose container is missing or stopped — both checks need
// the container to exist (1) and run (2). A missing token-store entry
// is reported by checkAnyGHToken already; this check covers the
// container-side propagation of an existing token.
func checkRepoTokenForwarding() []Problem {
	if _, err := exec.LookPath("incus"); err != nil {
		return nil
	}
	reg, err := registry.Load()
	if err != nil || len(reg.Repos) == 0 {
		return nil
	}
	var ps []Problem
	for i := range reg.Repos {
		repo := reg.Repos[i]
		if repo.BaseContainerName == "" {
			continue
		}
		// Only meaningful when the user actually configured a per-repo
		// PAT — otherwise checkAnyGHToken's warning is the right surface.
		if _, found, _ := tokenstore.GetAt(paths.SlugEnvPath(repo.Name), tokenstore.GHTokenEnv); !found {
			continue
		}
		ps = append(ps, checkContainerEnvGHToken(repo))
		ps = append(ps, checkContainerCredentialHelper(repo))
	}
	return ps
}

func checkContainerEnvGHToken(repo registry.Repo) Problem {
	val, err := incus.ConfigGet(repo.BaseContainerName, "environment.GH_TOKEN")
	if err != nil {
		return Problem{
			Severity: Warn,
			Title:    fmt.Sprintf("could not read environment.GH_TOKEN on %s", repo.BaseContainerName),
			Detail:   err.Error(),
		}
	}
	if val == "" {
		return Problem{
			Severity: Warn,
			Title:    fmt.Sprintf("environment.GH_TOKEN not set on %s", repo.BaseContainerName),
			Detail:   "per-repo PAT exists but is not forwarded into the container",
			Fix:      fmt.Sprintf("ahjo repo set-token %s  # re-applies environment.GH_TOKEN to every container", repo.Aliases[0]),
		}
	}
	return Problem{Severity: OK, Title: fmt.Sprintf("environment.GH_TOKEN forwarded on %s", repo.BaseContainerName)}
}

func checkContainerCredentialHelper(repo registry.Repo) Problem {
	status, err := incus.ContainerStatus(repo.BaseContainerName)
	if err != nil || !strings.EqualFold(status, "Running") {
		return Problem{
			Severity: OK,
			Title:    fmt.Sprintf("git credential helper check skipped on %s (not running)", repo.BaseContainerName),
		}
	}
	out, err := exec.Command(
		"incus", "exec", repo.BaseContainerName, "--user", "1000",
		"--env", "HOME=/home/ubuntu",
		"--", "git", "config", "--global", "--get", "credential.https://github.com.helper",
	).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return Problem{
			Severity: Warn,
			Title:    fmt.Sprintf("git credential helper not configured on %s", repo.BaseContainerName),
			Detail:   "raw `git clone/fetch` over HTTPS will prompt for credentials in this container",
			Fix:      fmt.Sprintf("ahjo ssh %s -- gh auth setup-git  # one-shot; or `ahjo repo rm %s && ahjo repo add` to rebuild", repo.Aliases[0], repo.Aliases[0]),
		}
	}
	return Problem{Severity: OK, Title: fmt.Sprintf("git credential helper configured on %s", repo.BaseContainerName)}
}

// checkGitIdentity reports whether ahjo can resolve a host git identity to
// seed inside containers (`/home/ubuntu/.gitconfig`). Identical resolution
// path as repoAddSetup, so a green here means `ahjo repo add` won't trip on
// the same lookup later.
func checkGitIdentity() Problem {
	id, err := git.ResolveHost()
	if err != nil {
		return Problem{
			Severity: Fail,
			Title:    "no host git identity for in-container commits",
			Fix:      `git config --global user.name "Your Name" && git config --global user.email "you@example.com"  # or: gh auth login`,
		}
	}
	return Problem{Severity: OK, Title: fmt.Sprintf("git identity (%s): %s <%s>", id.Source, id.Name, id.Email)}
}

func checkStoragePool() Problem {
	if _, err := exec.LookPath("incus"); err != nil {
		return Problem{Severity: Warn, Title: "skipped: incus storage check (incus missing)"}
	}
	driver, err := incus.StoragePoolDriver()
	if err != nil {
		return Problem{Severity: Warn, Title: "could not query incus storage", Detail: err.Error()}
	}
	if driver == "" {
		return Problem{Severity: Warn, Title: "no default incus storage pool", Fix: "incus admin init"}
	}
	if driver != "btrfs" && driver != "zfs" {
		return Problem{
			Severity: Warn,
			Title:    "incus storage driver is " + driver,
			Detail:   "btrfs or zfs is recommended for fast COW container creation",
		}
	}
	return Problem{Severity: OK, Title: "incus storage driver: " + driver}
}

func checkAhjoBase() Problem {
	if _, err := exec.LookPath("incus"); err != nil {
		return Problem{Severity: Warn, Title: "skipped: ahjo-base image check (incus missing)"}
	}
	exists, err := incus.ImageAliasExists("ahjo-base")
	if err != nil {
		return Problem{Severity: Warn, Title: "could not query incus image aliases", Detail: err.Error()}
	}
	if !exists {
		return Problem{
			Severity: Warn,
			Title:    "ahjo-base image not built",
			Fix:      "ahjo init  # builds ahjo-base via the ahjo-runtime devcontainer Feature pipeline",
		}
	}
	return Problem{Severity: OK, Title: "ahjo-base image present"}
}

// Format renders a Problem as a single line for printing.
func Format(p Problem) string {
	mark := "[ok]  "
	switch p.Severity {
	case Warn:
		mark = "[warn]"
	case Fail:
		mark = "[fail]"
	}
	out := fmt.Sprintf("%s %s", mark, p.Title)
	if p.Detail != "" {
		out += "\n       " + p.Detail
	}
	if p.Fix != "" {
		out += "\n       fix: " + p.Fix
	}
	return out
}

// Worst returns the most severe level encountered.
func Worst(ps []Problem) Severity {
	worst := OK
	for _, p := range ps {
		if p.Severity > worst {
			worst = p.Severity
		}
	}
	return worst
}
