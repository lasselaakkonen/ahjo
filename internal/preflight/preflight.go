// Package preflight runs read-only environmental checks for `ahjo doctor`.
// Each check returns a Problem with a fix hint; ahjo never auto-installs.
package preflight

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
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
