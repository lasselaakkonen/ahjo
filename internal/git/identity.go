package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// Env-var bridge from the Mac shim to the in-VM relay. The Mac side
// resolves identity before relay (where `git config --global` and `gh`
// actually live) and prepends these vars to the relay command; this
// side reads them. Linux-native callers (no shim, no relay) won't see
// the vars and fall through to the host-resolution path, which Just
// Works because `git` and `gh` are on the same machine.
const (
	envHostName   = "AHJO_HOST_GIT_NAME"
	envHostEmail  = "AHJO_HOST_GIT_EMAIL"
	envHostSource = "AHJO_HOST_GIT_SOURCE"
)

// EnvKeys returns the env-var names the Mac shim sets when bridging
// identity into the VM. Exported so the shim can stay in sync with the
// reader here without re-declaring the constants.
func EnvKeys() (name, email, source string) {
	return envHostName, envHostEmail, envHostSource
}

// ResolveHost picks an identity for `/home/ubuntu/.gitconfig` from, in order:
//  1. the AHJO_HOST_GIT_* env-var bridge (set by the Mac shim before
//     relaying into the VM — that's the only place `git config` and
//     `gh` are actually available on a Mac-driven invocation),
//  2. the host's `git config --global user.name` + `user.email`,
//  3. `gh api user` (when the GitHub CLI is installed and authenticated).
//
// Returns an error when no source yields both a name and an email — the
// caller is expected to surface the message verbatim so users know which
// fix command applies.
func ResolveHost() (Identity, error) {
	if id, ok := fromBridgeEnv(); ok {
		return id, nil
	}
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

// fromBridgeEnv reads the AHJO_HOST_GIT_* envelope the Mac shim sets
// before relaying. Both name and email must be present; source falls
// back to a "Mac host" label so log lines stay informative when the
// shim didn't pass one.
func fromBridgeEnv() (Identity, bool) {
	name := strings.TrimSpace(os.Getenv(envHostName))
	email := strings.TrimSpace(os.Getenv(envHostEmail))
	if name == "" || email == "" {
		return Identity{}, false
	}
	source := strings.TrimSpace(os.Getenv(envHostSource))
	if source == "" {
		source = "Mac host"
	}
	return Identity{Name: name, Email: email, Source: source}, true
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
