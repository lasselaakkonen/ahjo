// Package devcontainer applies devcontainer Features to Incus system
// containers. Phase 1 of adopt-devcontainer-spec.md scopes this package to
// the apply path against an already-extracted Feature dir (the embedded
// ahjo-runtime is the only Phase 1 caller). Phase 2 adds the OCI fetch path
// for user-supplied Features under devcontainer.json's `features` field.
package devcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// applyTimeout caps how long a single Feature's install.sh may run. The
// devcontainer Features spec recommends a generous window; ahjo-runtime's
// apt + nodejs install runs about 1m on a warm cache, 3m on a cold pull —
// 10m gives a comfortable margin while still bounding pathological hangs.
const applyTimeout = 10 * time.Minute

// Feature is one Feature ready to apply: a directory containing
// devcontainer-feature.json + install.sh, plus an ID and option map.
//
// ID is the human-readable form (the OCI ref for user Features, e.g.
// "ghcr.io/devcontainers/features/go:1", or a short slug for ahjo's
// embedded Features, e.g. "ahjo-runtime"). It surfaces verbatim in
// error/warning messages so the user can find the offending Feature
// in their devcontainer.json. The in-container temp-dir path is
// derived from the ID via SafeRefDir at Apply time — callers don't
// pre-sanitize.
//
// Options become ALL_CAPS env vars per the spec.
type Feature struct {
	ID      string
	Dir     string
	Options map[string]string
}

// SafeRefDir maps a Feature ID (typically an OCI ref like
// "ghcr.io/devcontainers/features/go:1") to a filesystem-safe basename
// for tmp dirs and in-container paths. Any byte outside [a-zA-Z0-9._-]
// becomes "-". Results are ephemeral and only need to be stable +
// unique per input — collision concerns are handled at the caller via
// per-invocation tmp roots.
//
// Exported because both the cli/features.go fetch closure (user repo
// Features) and the devcontainer/build.go base-bake path need the same
// transform; keeping the helper here means the two callers can't drift.
func SafeRefDir(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// Metadata is the subset of devcontainer-feature.json ahjo reads.
// dependsOn / installsAfter / containerEnv feed the Phase 2b resolver;
// Options feeds default-value materialization (see ApplyOptionDefaults);
// the Docker-flavored fields split two ways — mounts/privileged are
// hard-rejected (they ask for runtime semantics ahjo cannot supply
// without breaking the Feature), the rest are warned-and-ignored
// (they map to Docker-runtime concepts that Incus system containers
// either don't need or already provide via systemd).
//
// Spec reference: https://containers.dev/implementors/features/
type Metadata struct {
	ID            string                    `json:"id"`
	DependsOn     map[string]map[string]any `json:"dependsOn"`
	InstallsAfter []string                  `json:"installsAfter"`
	ContainerEnv  map[string]string         `json:"containerEnv"`
	LegacyIds     []string                  `json:"legacyIds"`
	Options       map[string]OptionSpec     `json:"options"`

	// Hard-rejected — the Feature genuinely depends on these to work.
	Mounts     []any `json:"mounts"`
	Privileged *bool `json:"privileged"`

	// Warned-and-ignored — Docker-runtime hints with no Incus equivalent
	// under ahjo's profile, or already provided by systemd.
	CapAdd      []string `json:"capAdd"`
	SecurityOpt []string `json:"securityOpt"`
	Init        *bool    `json:"init"`
	Entrypoint  string   `json:"entrypoint"`
}

// OptionSpec is the per-option block under devcontainer-feature.json's
// `options`. ahjo only needs the default value — the spec also defines
// `type` / `proposals` / `description` for UI tooling, none of which
// apply when ahjo is the runner.
type OptionSpec struct {
	Default any `json:"default"`
}

// ApplyOptionDefaults merges Feature-declared option defaults into the
// user-supplied option map. Without this step, an option that the user
// omits goes through as "unset" (which the spec maps to "" in the env
// envelope), and Features that branch on a default keyword break at
// install time — the canonical example is the curated `git:1` Feature,
// whose install.sh does `GIT_VERSION=${VERSION}` and crashes with
// "Invalid git version:" when VERSION is empty, even though the
// Feature's own metadata declares `version` defaults to `os-provided`.
//
// User-supplied values win over metadata defaults (the user explicitly
// passed them); metadata defaults fill in only the keys the user
// didn't mention. The map order is irrelevant for correctness — env
// vars are merged by key in the Apply path — but the implementation
// keeps user keys in their original form so downstream JSON round-trips
// don't surprise anyone reading logs.
func ApplyOptionDefaults(userOpts map[string]any, meta *Metadata) map[string]any {
	if meta == nil || len(meta.Options) == 0 {
		if userOpts == nil {
			return nil
		}
		// Defensive copy — callers shouldn't see future map mutations
		// land on their input.
		out := make(map[string]any, len(userOpts))
		for k, v := range userOpts {
			out[k] = v
		}
		return out
	}
	merged := make(map[string]any, len(meta.Options)+len(userOpts))
	for k, spec := range meta.Options {
		if spec.Default != nil {
			merged[k] = spec.Default
		}
	}
	for k, v := range userOpts {
		merged[k] = v
	}
	return merged
}

// ReadMetadata parses a Feature dir's devcontainer-feature.json and
// returns the parsed metadata + an error if any Docker-flavored fields
// are declared. Used by the resolver before extracting deps.
func ReadMetadata(featureDir, featureID string) (*Metadata, error) {
	metaPath := filepath.Join(featureDir, "devcontainer-feature.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", metaPath, err)
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metaPath, err)
	}
	if err := rejectDockerFields(&m, featureID); err != nil {
		return nil, err
	}
	return &m, nil
}

// rejectDockerFields refuses to apply a Feature whose own
// devcontainer-feature.json declares a field that asks for runtime
// behavior ahjo cannot supply without breaking the Feature. The set is
// narrow on purpose — Docker-flavored *hints* (capAdd / securityOpt /
// init / entrypoint) are handled by noteIgnoredDockerFields instead,
// so a Feature that would install correctly on ahjo (e.g. the curated
// `go` Feature that declares SYS_PTRACE for delve) still runs with a
// warning rather than a hard error.
//
// `mounts` is rejected because the Feature genuinely relies on the
// declared paths being present at runtime (docker-in-docker's
// /var/lib/docker volume, docker-outside-of-docker's docker.sock bind,
// nix's /nix store, kubectl-helm-minikube's minikube state). Letting
// install.sh succeed without the mount would leave a Feature that
// looks installed but is broken on first use.
//
// `privileged` is rejected because granting full host capabilities
// inverts ahjo's container isolation model — there's no warning that
// makes that safe to ignore.
func rejectDockerFields(m *Metadata, featureID string) error {
	if len(m.Mounts) > 0 {
		return fmt.Errorf("Feature %s declares `mounts` — Docker-flavored, ahjo cannot supply mounts to a Feature without changing the Incus profile; this is an unsupported topology", featureID)
	}
	if m.Privileged != nil && *m.Privileged {
		return fmt.Errorf("Feature %s declares `privileged: true` — granting full host capabilities to an ahjo container would defeat its isolation model; this is an unsupported topology", featureID)
	}
	return nil
}

// noteIgnoredDockerFields returns one human-readable warning line per
// Docker-runtime hint the Feature declares but ahjo can safely ignore.
// Callers print each line through their own writer (typically prefixed
// "warn:") right before applying the Feature, so the user sees exactly
// which knobs are being dropped and why.
//
// Each warning explains *what Docker would do* and *why it's a no-op on
// ahjo's Incus system container*, so future-you (debugging "the Feature
// claimed it needed X and we ignored it — was that fine?") has the
// reasoning inline rather than buried in a design doc.
//
// Known values get specific text. `SYS_PTRACE`, `seccomp=unconfined`,
// and `label=disable` show up across the curated catalog (go, rust,
// docker-outside-of-docker) and have well-understood semantics worth
// naming; anything else falls through to a generic "no Incus equivalent"
// note that still tells the user the field was dropped.
func noteIgnoredDockerFields(m *Metadata, featureID string) []string {
	var notes []string

	if m.Init != nil && *m.Init {
		notes = append(notes, fmt.Sprintf(
			"Feature %s declares `init: true` — Docker would inject tini as PID 1 to reap zombies and forward signals; ahjo's container runs systemd as PID 1 and handles both natively, so this is a no-op",
			featureID,
		))
	}

	for _, cap := range m.CapAdd {
		notes = append(notes, capAddNote(featureID, cap))
	}

	for _, opt := range m.SecurityOpt {
		notes = append(notes, securityOptNote(featureID, opt))
	}

	if m.Entrypoint != "" {
		notes = append(notes, fmt.Sprintf(
			"Feature %s declares `entrypoint: %q` — Docker would run this script as the container CMD on start; ahjo runs systemd as PID 1 and ignores Feature entrypoints. install.sh still runs at apply time, but anything the entrypoint script does at runtime (service bring-up, env wiring) won't happen — if the Feature relies on it, expect runtime breakage",
			featureID, m.Entrypoint,
		))
	}

	return notes
}

func capAddNote(featureID, cap string) string {
	switch cap {
	case "SYS_PTRACE":
		return fmt.Sprintf(
			"Feature %s declares `capAdd: [SYS_PTRACE]` — Docker grants this so debuggers (delve, lldb, gdb) can attach across PID namespaces. On ahjo's Incus container, in-container ptrace already works (debugger and target share the same user namespace), so this is a no-op for the typical debugger-in-same-container case",
			featureID,
		)
	default:
		return fmt.Sprintf(
			"Feature %s declares `capAdd: [%s]` — Docker capability hints have no per-Feature equivalent under ahjo's Incus profile; the Feature install will run but anything at runtime that requires this capability may surface as EPERM",
			featureID, cap,
		)
	}
}

func securityOptNote(featureID, opt string) string {
	switch opt {
	case "seccomp=unconfined":
		return fmt.Sprintf(
			"Feature %s declares `securityOpt: [seccomp=unconfined]` — Docker would drop its default seccomp filter for this container. Incus has its own seccomp policy (set by the Incus profile, not per-Feature) and no equivalent override; if a syscall is blocked you'll see EPERM at runtime, but most Go/Rust workloads run fine under Incus's defaults",
			featureID,
		)
	case "label=disable":
		return fmt.Sprintf(
			"Feature %s declares `securityOpt: [label=disable]` — Docker would disable SELinux labels. ahjo's container runtime doesn't enforce SELinux on the workload, so this is a no-op",
			featureID,
		)
	default:
		return fmt.Sprintf(
			"Feature %s declares `securityOpt: [%s]` — Docker-flavored, no Incus equivalent under ahjo's profile; ignoring",
			featureID, opt,
		)
	}
}

// Apply pushes f into the container, runs its install.sh as root with the
// devcontainer Feature env vars set, and cleans up the in-container temp
// dir on success and on failure. env is the user-identity envelope from
// the caller (typically `_REMOTE_USER`/`_REMOTE_USER_HOME`/`_CONTAINER_USER`/
// `_CONTAINER_USER_HOME`); options become ALL_CAPS env vars per the
// devcontainer Features spec.
func Apply(container string, f Feature, env map[string]string, out io.Writer) error {
	if f.ID == "" {
		return errors.New("Feature.ID required")
	}
	if f.Dir == "" {
		return errors.New("Feature.Dir required")
	}
	meta, err := validate(f)
	if err != nil {
		return err
	}
	for _, note := range noteIgnoredDockerFields(meta, f.ID) {
		fmt.Fprintln(out, "warn: "+note)
	}

	// The in-container temp-dir name has to be filesystem-safe, but
	// f.ID is the human-readable form for messages — apply the
	// transform here rather than asking every Feature constructor to
	// pre-sanitize.
	containerPath := "/tmp/feature-" + SafeRefDir(f.ID)
	defer cleanupRemote(container, containerPath)

	// `incus file push --recursive <dir> <c>/<path>` puts <dir>'s contents
	// at <c>/<path>/<basename(dir)>; pre-create the parent and push the
	// dir contents so the install.sh lands at <containerPath>/install.sh
	// regardless of what the host tempdir is named.
	if _, err := incus.Exec(container, "rm", "-rf", containerPath); err != nil {
		return fmt.Errorf("clear stale %s: %w", containerPath, err)
	}
	if _, err := incus.Exec(container, "mkdir", "-p", containerPath); err != nil {
		return fmt.Errorf("mkdir %s: %w", containerPath, err)
	}
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return fmt.Errorf("read feature dir %s: %w", f.Dir, err)
	}
	for _, e := range entries {
		host := filepath.Join(f.Dir, e.Name())
		if err := incus.FilePushRecursive(container, host, containerPath+"/"); err != nil {
			return fmt.Errorf("push %s: %w", host, err)
		}
	}

	// Merge identity env + Feature options. Options spec'd as ALL_CAPS
	// keys per the devcontainer Features spec.
	envv := map[string]string{}
	for k, v := range env {
		envv[k] = v
	}
	for k, v := range f.Options {
		envv[k] = v
	}

	// Override HOME/USER/LOGNAME for install.sh so it sees a root-shaped
	// home. The container has environment.HOME=/home/ubuntu (the ubuntu
	// user's home, used by every other exec). install.sh runs as root and
	// expects a root home: upstream Features write /etc/profile.d/
	// 00-restore-env.sh via `${PATH//$(sh -lc 'echo $PATH')/\$PATH}`, a
	// substitution that assumes install.sh's PATH equals sh -lc's PATH.
	// With HOME=/home/ubuntu, sh -lc sources /home/ubuntu/.profile and
	// picks up $HOME/.local/bin — making login-shell PATH ⊃ install.sh
	// PATH, the substitution finds no match, and the file lands as a
	// literal pre-Feature PATH that shadows environment.PATH on every
	// later login. /root has no .profile by default, so sh -lc returns
	// install.sh's PATH and the substitution yields `$PATH` (the no-op
	// the Feature intended).
	envv["HOME"] = "/root"
	envv["USER"] = "root"
	envv["LOGNAME"] = "root"

	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	args := []string{"exec", container}
	for _, k := range sortedKeys(envv) {
		args = append(args, "--env", k+"="+envv[k])
	}
	args = append(args, "--", "bash", containerPath+"/install.sh")

	cmd := exec.CommandContext(ctx, "incus", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("Feature %s install.sh timed out after %s", f.ID, applyTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("Feature %s install.sh: exit %d", f.ID, ee.ExitCode())
		}
		return fmt.Errorf("Feature %s install.sh: %w", f.ID, err)
	}

	// Persist the Feature's containerEnv as Incus environment.* keys so
	// every subsequent `incus exec` (lifecycle commands, warm-install,
	// user shell, next Feature's install.sh) sees the new PATH/GOROOT/…
	// `${VAR}` is expanded against the container's current login env so
	// that values like `PATH: /usr/local/go/bin:/go/bin:${PATH}` resolve
	// to a literal path string — Incus's `environment.PATH` accepts no
	// interpolation and a literal `${PATH}` would brick every later exec.
	// Reading via `bash -lc env` picks up additions from earlier Features
	// (their own environment.* values are inherited by this exec), so a
	// chain of Features each appending to `${PATH}` composes correctly.
	current, err := readLoginEnv(container)
	if err != nil {
		// Soft-fail: a Feature without ${VAR} references in its
		// containerEnv still applies correctly with current=nil
		// (os.Expand returns "" for unknown names). Only the
		// PATH-augmenting Features lose their additions on this path,
		// and the user sees the warning.
		fmt.Fprintf(out, "warn: read container env for %s containerEnv expansion: %v\n", f.ID, err)
	}
	expanded := expandContainerEnv(meta.ContainerEnv, current)
	for _, k := range sortedKeys(expanded) {
		if err := incus.ConfigSet(container, "environment."+k, expanded[k]); err != nil {
			return fmt.Errorf("Feature %s: persist containerEnv %s: %w", f.ID, k, err)
		}
	}
	return nil
}

func validate(f Feature) (*Metadata, error) {
	installPath := filepath.Join(f.Dir, "install.sh")
	if _, err := os.Stat(installPath); err != nil {
		return nil, fmt.Errorf("Feature %s missing install.sh: %w", f.ID, err)
	}
	return ReadMetadata(f.Dir, f.ID)
}

// cleanupRemote removes the in-container temp dir. Best-effort: a failure
// here means the build container is about to be deleted anyway, so we log
// and move on rather than masking the original Apply error.
func cleanupRemote(container, path string) {
	_, _ = incus.Exec(container, "rm", "-rf", path)
}

// readLoginEnv runs `bash -lc env` in the container and parses the output
// into a map. Login shell so that /etc/profile.d/*.sh is sourced — that's
// where prior Features persist their PATH additions (via the
// 00-restore-env.sh trick), and the next Feature's ${PATH} expansion must
// see them. An error is returned untouched; the caller decides whether to
// soft-fail.
func readLoginEnv(container string) (map[string]string, error) {
	out, err := incus.Exec(container, "bash", "-lc", "env")
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		env[line[:i]] = line[i+1:]
	}
	return env, nil
}

// expandContainerEnv substitutes ${VAR} (and $VAR) references in each
// containerEnv value against current. Unset names expand to "" — matching
// shell semantics and devcontainers/cli, where a Feature that writes
// `PATH: ${PATH}:/extra` against an empty current ends up with
// `PATH=:/extra` (harmless leading empty entry, vs. a literal `${PATH}`
// which would brick every later exec).
//
// Nil or empty raw returns nil.
func expandContainerEnv(raw, current map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = os.Expand(v, func(name string) string {
			return current[name]
		})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
