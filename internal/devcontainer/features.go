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
	"time"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// applyTimeout caps how long a single Feature's install.sh may run. The
// devcontainer Features spec recommends a generous window; ahjo-runtime's
// apt + nodejs install runs about 1m on a warm cache, 3m on a cold pull —
// 10m gives a comfortable margin while still bounding pathological hangs.
const applyTimeout = 10 * time.Minute

// Feature is one Feature ready to apply: a directory containing
// devcontainer-feature.json + install.sh, plus a normalized ID for naming
// the in-container temp dir. Options become ALL_CAPS env vars per the spec.
type Feature struct {
	ID      string
	Dir     string
	Options map[string]string
}

// metadata is the subset of devcontainer-feature.json we read for
// validation. Phase 2 will extend this with dependsOn / installsAfter /
// containerEnv. The Docker-flavored fields are listed only so we can
// reject Features that declare them; we never honor them.
type metadata struct {
	ID          string   `json:"id"`
	Mounts      []any    `json:"mounts"`
	Privileged  *bool    `json:"privileged"`
	CapAdd      []string `json:"capAdd"`
	SecurityOpt []string `json:"securityOpt"`
	Init        *bool    `json:"init"`
	Entrypoint  string   `json:"entrypoint"`
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
	if err := validate(f); err != nil {
		return err
	}

	containerPath := "/tmp/feature-" + f.ID
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
	return nil
}

func validate(f Feature) error {
	metaPath := filepath.Join(f.Dir, "devcontainer-feature.json")
	installPath := filepath.Join(f.Dir, "install.sh")
	if _, err := os.Stat(metaPath); err != nil {
		return fmt.Errorf("Feature %s missing devcontainer-feature.json: %w", f.ID, err)
	}
	if _, err := os.Stat(installPath); err != nil {
		return fmt.Errorf("Feature %s missing install.sh: %w", f.ID, err)
	}
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", metaPath, err)
	}
	var m metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("parse %s: %w", metaPath, err)
	}
	// Reject Docker-flavored fields with explicit error citing the Feature
	// ID. Phase 2 reuses this guard for user-supplied Features; we apply
	// it to the embedded ahjo-runtime too so a regression in our own
	// metadata fails loudly rather than silently honoring the wrong
	// semantics.
	if len(m.Mounts) > 0 {
		return fmt.Errorf("Feature %s declares `mounts` — Docker-flavored, not supported by ahjo", f.ID)
	}
	if m.Privileged != nil && *m.Privileged {
		return fmt.Errorf("Feature %s declares `privileged` — Docker-flavored, not supported by ahjo", f.ID)
	}
	if len(m.CapAdd) > 0 {
		return fmt.Errorf("Feature %s declares `capAdd` — Docker-flavored, not supported by ahjo", f.ID)
	}
	if len(m.SecurityOpt) > 0 {
		return fmt.Errorf("Feature %s declares `securityOpt` — Docker-flavored, not supported by ahjo", f.ID)
	}
	if m.Init != nil && *m.Init {
		return fmt.Errorf("Feature %s declares `init` — Docker-flavored, not supported by ahjo", f.ID)
	}
	if m.Entrypoint != "" {
		return fmt.Errorf("Feature %s declares `entrypoint` — Docker-flavored, not supported by ahjo", f.ID)
	}
	return nil
}

// cleanupRemote removes the in-container temp dir. Best-effort: a failure
// here means the build container is about to be deleted anyway, so we log
// and move on rather than masking the original Apply error.
func cleanupRemote(container, path string) {
	_, _ = incus.Exec(container, "rm", "-rf", path)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
