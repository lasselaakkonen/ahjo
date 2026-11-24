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
	if p, ok := checkSharedRepoEnvOnMac(); ok {
		ps = append(ps, p)
	}
	ps = append(ps, checkGitIdentity())
	ps = append(ps, checkStoragePool())
	ps = append(ps, checkNetworkHardening())
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
	if v, _ := tokenstore.Lookup(tokenstore.TokenEnv); v == "" {
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
	if _, isMac := paths.MacHostHome(); isMac {
		// Per-repo PATs live in the user's login Keychain on macOS, not on
		// disk. The in-VM ahjo has no Keychain visibility from inside the VM;
		// the Mac-side doctor block (checkKeychainPATs) surveys them.
		return Problem{Severity: OK, Title: "per-repo PATs surveyed Mac-side; see host checks above"}
	}
	reg, err := registry.Load()
	if err != nil {
		return Problem{Severity: Warn, Title: "could not load registry for GH_TOKEN survey", Detail: err.Error()}
	}
	globalSet := false
	if v, _ := tokenstore.Lookup(tokenstore.GHTokenEnv); v != "" {
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

// checkSharedRepoEnvOnMac warns when, on a Mac users' VM, the new shared
// repo-env path (~/.ahjo-shared/repo-env/<slug>.env) still has plaintext PAT
// files. After the Keychain move, PATs should live in the macOS login
// Keychain only; on-disk copies defeat the "grep ~ for ghp_… finds nothing"
// goal. Per the project's no-runtime-migration rule, the user self-migrates
// by running `ahjo repo set-token <alias>` once Mac-side and then removing
// the leftover dir.
func checkSharedRepoEnvOnMac() (Problem, bool) {
	if _, isMac := paths.MacHostHome(); !isMac {
		return Problem{}, false
	}
	shared := paths.RepoEnvDir()
	entries, err := os.ReadDir(shared)
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
		Title:    fmt.Sprintf("%d per-repo PAT file(s) on disk at %s", len(slugs), shared),
		Detail: "on macOS, per-repo PATs should live in the login Keychain — not in the " +
			"shared host directory where a backup or `grep ~` would find them. " +
			"Affected repos: " + strings.Join(slugs, ", "),
		Fix: "for each: ahjo repo set-token <alias>  # stores in Keychain via the Mac shim\n" +
			"       then: rm -rf " + shared,
	}, true
}

// checkRepoTokenForwarding covers the container side of GitHub auth for
// each registered repo:
//
//  1. On every host (Mac VM included): no container still carries the PAT
//     in its Incus config. Older ahjo versions persisted it as
//     environment.GH_TOKEN / environment.GITHUB_TOKEN; it is now injected
//     per exec and never stored. See checkPersistedRepoToken.
//  2. On standalone Linux, for repos with a per-repo PAT:
//     credential.https://github.com.helper configured in the in-container
//     /home/ubuntu/.gitconfig (written by `gh auth setup-git`), so git
//     over HTTPS picks up the injected GH_TOKEN. On Mac users' VM the PAT
//     lives in the host Keychain, which the in-VM side cannot survey, so
//     the Mac-side doctor block is the source of truth there.
//
// A missing token-store entry is reported by checkAnyGHToken already.
func checkRepoTokenForwarding() []Problem {
	if _, err := exec.LookPath("incus"); err != nil {
		return nil
	}
	reg, err := registry.Load()
	if err != nil || len(reg.Repos) == 0 {
		return nil
	}
	ps := checkPersistedRepoToken(reg)
	if _, isMac := paths.MacHostHome(); isMac {
		return ps
	}
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
		ps = append(ps, checkContainerCredentialHelper(repo))
	}
	return ps
}

// checkPersistedRepoToken warns for each existing container (base and branch
// containers of every repo) whose Incus config still holds the PAT under a
// key older ahjo versions wrote. Clean containers produce no row, and a key
// that cannot be read is skipped: this is a leftover sweep, not a probe of
// incus health.
func checkPersistedRepoToken(reg *registry.Registry) []Problem {
	var ps []Problem
	for i := range reg.Repos {
		repo := reg.Repos[i]
		alias := repo.Name
		if len(repo.Aliases) > 0 {
			alias = repo.Aliases[0]
		}
		names := []string{repo.BaseContainerName}
		for j := range reg.Branches {
			if reg.Branches[j].Repo == repo.Name {
				names = append(names, reg.Branches[j].IncusName)
			}
		}
		seen := map[string]struct{}{}
		for _, name := range names {
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			if exists, err := incus.ContainerExists(name); err != nil || !exists {
				continue
			}
			gh, err1 := incus.ConfigGet(name, "environment.GH_TOKEN")
			github, err2 := incus.ConfigGet(name, "environment.GITHUB_TOKEN")
			if err1 != nil && err2 != nil {
				continue
			}
			if p, ok := persistedTokenProblem(name, alias, gh, github); ok {
				ps = append(ps, p)
			}
		}
	}
	return ps
}

// persistedTokenProblem is checkPersistedRepoToken's decision: a Warn when
// either legacy key holds a value. gh / github are the raw config values;
// they are only tested for emptiness and never copied into the Problem.
func persistedTokenProblem(name, alias, gh, github string) (Problem, bool) {
	if gh == "" && github == "" {
		return Problem{}, false
	}
	return Problem{
		Severity: Warn,
		Title:    fmt.Sprintf("GitHub PAT still stored in Incus config on %s", name),
		Detail:   "older ahjo versions persisted the PAT as environment.GH_TOKEN; it is now injected per session",
		Fix:      fmt.Sprintf("ahjo repo set-token %s  # or any ahjo shell/claude on the repo; then rotate the PAT", alias),
	}, true
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

// checkNetworkHardening warns when incusbr0 lacks the egress ACL or the
// default profile's NIC lacks port isolation (installs that have not run
// `ahjo update` since the hardening shipped). Like the other incus checks it
// is skipped when incus is missing, which is also the case on a bare macOS
// host; the incus-missing failure is already reported by checkBinary.
func checkNetworkHardening() Problem {
	if _, err := exec.LookPath("incus"); err != nil {
		return Problem{Severity: Warn, Title: "skipped: incusbr0 hardening check (incus missing)"}
	}
	status, err := incus.CheckNetworkHardening()
	return networkHardeningProblem(status, err)
}

// networkHardeningProblem maps a hardening status (or the error from
// obtaining it) to a Problem. An error is never reported as OK.
func networkHardeningProblem(status incus.NetworkHardeningStatus, err error) Problem {
	if err != nil {
		return Problem{Severity: Warn, Title: "could not check incusbr0 hardening", Detail: err.Error()}
	}
	if !status.OK() {
		return Problem{
			Severity: Warn,
			Title:    "incusbr0 egress firewall or container isolation missing",
			Detail: status.Reason + ". Until fixed, containers can reach the host, " +
				"the Mac, the LAN and each other.",
			Fix: "ahjo update",
		}
	}
	return Problem{Severity: OK, Title: "incusbr0 egress firewall and container isolation active"}
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
