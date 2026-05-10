package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Identity is the git author identity ahjo seeds inside a fresh branch
// container. Source records where Name/Email came from so callers can
// surface the provenance ("from your host gitconfig", "from GitHub").
type Identity struct {
	Name   string
	Email  string
	Source string
}

// ResolveHost picks an identity for `/home/ubuntu/.gitconfig` from, in order:
//  1. the host's `git config --global user.name` + `user.email`,
//  2. `gh api user` (when the GitHub CLI is installed and authenticated).
//
// Returns an error when neither source yields both a name and an email — the
// caller is expected to surface the message verbatim so users know which
// fix command applies.
func ResolveHost() (Identity, error) {
	if id, ok := fromHostGitconfig(); ok {
		return id, nil
	}
	if id, err := fromGH(); err == nil {
		return id, nil
	} else if !errors.Is(err, errGHUnavailable) {
		return Identity{}, err
	}
	return Identity{}, errors.New(
		"no git identity available on the host\n" +
			"  fix: set one of\n" +
			"    git config --global user.name \"Your Name\"\n" +
			"    git config --global user.email \"you@example.com\"\n" +
			"  or authenticate the GitHub CLI:\n" +
			"    gh auth login")
}

func fromHostGitconfig() (Identity, bool) {
	name := gitConfigGlobal("user.name")
	email := gitConfigGlobal("user.email")
	if name == "" || email == "" {
		return Identity{}, false
	}
	return Identity{Name: name, Email: email, Source: "host gitconfig"}, true
}

func gitConfigGlobal(key string) string {
	out, err := exec.Command("git", "config", "--global", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var errGHUnavailable = errors.New("gh CLI not available or not authenticated")

// fromGH queries `gh api user` for name + email. GitHub hides email by
// default; when `.email` is null we synthesize the canonical noreply form
// (`<id>+<login>@users.noreply.github.com`) so commits land on the right
// account without leaking a private address.
func fromGH() (Identity, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return Identity{}, errGHUnavailable
	}
	out, err := exec.Command("gh", "api", "user").Output()
	if err != nil {
		return Identity{}, errGHUnavailable
	}
	var u struct {
		Login string  `json:"login"`
		ID    int64   `json:"id"`
		Name  string  `json:"name"`
		Email *string `json:"email"`
	}
	if err := json.Unmarshal(out, &u); err != nil {
		return Identity{}, fmt.Errorf("parse gh api user: %w", err)
	}
	if u.Login == "" {
		return Identity{}, errGHUnavailable
	}
	name := u.Name
	if name == "" {
		name = u.Login
	}
	email := ""
	if u.Email != nil {
		email = strings.TrimSpace(*u.Email)
	}
	if email == "" {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
	}
	return Identity{Name: name, Email: email, Source: "gh api user"}, nil
}
