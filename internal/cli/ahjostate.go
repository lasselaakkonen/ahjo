package cli

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/ahjostate"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

const (
	// AHJO.md is Claude instructions, so it lives in Claude's config dir and is
	// @imported by CLAUDE.md.
	claudeDirPath     = "/home/ubuntu/.claude"
	ahjoDocPath       = claudeDirPath + "/AHJO.md"
	claudeMdPath      = claudeDirPath + "/CLAUDE.md"
	ahjoDocImportLine = "@AHJO.md"

	// The state snapshots are ahjo's own data, not Claude config, so they live
	// under ahjo's dir. AHJO.md reaches the markdown via a home-relative @import.
	ahjoStateDirPath  = "/home/ubuntu/.ahjo"
	ahjoStatePath     = ahjoStateDirPath + "/ahjo-state.md"   // prose snapshot Claude reads
	ahjoStateJSONPath = ahjoStateDirPath + "/ahjo-state.json" // machine-readable twin the statusline parses
)

// ahjoDocContent is the static AHJO.md shipped with ahjo: it explains the three
// host↔container bridges and points Claude at ahjo-state.md for live state.
//
//go:embed AHJO.md
var ahjoDocContent string

// refreshAhjoState regenerates and pushes the ahjo-state snapshots (markdown +
// JSON) for the branch named by alias. Best-effort by contract: callers wire it
// after a successful mutation or before attach, and a failure here must never
// fail that primary command — it only warns.
func refreshAhjoState(alias string) {
	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: ahjo-state: load registry: %v\n", err)
		return
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return
	}
	containerName, err := resolveContainerName(br)
	if err != nil {
		return
	}
	var mirrorTarget string
	if repo := reg.FindRepo(br.Repo); repo != nil {
		mirrorTarget = repo.MacMirrorTarget
	}
	refreshAhjoStateByName(containerName, br.Slug, alias, mirrorTarget)
}

// refreshAhjoStateByName is the alias-free core. Used directly by `repo add`,
// where the branch isn't in the registry yet but the state is trivially "all
// off". Best-effort, same contract as refreshAhjoState.
func refreshAhjoStateByName(containerName, slug, alias, mirrorTarget string) {
	st, err := gatherAhjoState(containerName, slug, alias, mirrorTarget)
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: ahjo-state: %v\n", err)
		return
	}
	if err := pushAhjoState(containerName, st); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: ahjo-state push: %v\n", err)
	}
}

// gatherAhjoState collects the container's live bridge state into an
// ahjostate.State, format-agnostic — the caller renders it to markdown and JSON.
// Mirror on/off comes from the mirror disk device; expose from ports.json;
// forward from live proxy devices — the same sources `ls` reads.
func gatherAhjoState(containerName, slug, alias, mirrorTarget string) (ahjostate.State, error) {
	st := ahjostate.State{
		Slug:       slug,
		Alias:      alias,
		At:         time.Now(),
		MirrorRepo: mirrorRepoPath,
	}

	has, err := incus.HasDevice(containerName, mirrorDeviceName)
	if err != nil {
		return ahjostate.State{}, fmt.Errorf("query mirror device: %w", err)
	}
	st.MirrorOn = has
	if has {
		st.MirrorHostTarget = mirrorTarget
	}

	pp, err := ports.Load()
	if err != nil {
		return ahjostate.State{}, fmt.Errorf("load ports: %w", err)
	}
	st.Expose = ports.ExposedPairs(pp.AllocationsForSlug(slug))

	devs, err := incus.ListProxyDevices(containerName)
	if err != nil {
		return ahjostate.State{}, fmt.Errorf("list proxy devices: %w", err)
	}
	st.Forward = forwardPairs(devs)

	return st, nil
}

// installAhjoDoc writes the static AHJO.md into the container and makes CLAUDE.md
// import it. Run once at base-container creation; COW clones inherit both. The
// CLAUDE.md edit is idempotent and create-if-missing, so it survives a re-pushed
// CLAUDE.md and the case where the user has no global CLAUDE.md.
func installAhjoDoc(containerName string) error {
	if err := ensureContainerDir(containerName, claudeDirPath); err != nil {
		return err
	}
	if err := pushContainerFile(containerName, ahjoDocContent, ahjoDocPath); err != nil {
		return fmt.Errorf("push AHJO.md: %w", err)
	}
	// touch+grep+append keeps this idempotent and handles a missing CLAUDE.md.
	script := fmt.Sprintf(
		`f=%s; touch "$f"; grep -qF '%s' "$f" || printf '\n<!-- ahjo:managed -->\n%s\n' >> "$f"; chown 1000:1000 "$f"`,
		claudeMdPath, ahjoDocImportLine, ahjoDocImportLine,
	)
	if err := incus.ExecAs(containerName, 0, nil, "/", "bash", "-c", script); err != nil {
		return fmt.Errorf("import AHJO.md from CLAUDE.md: %w", err)
	}
	return nil
}

// pushAhjoState renders st to both snapshot formats and writes them into the
// container, owned by the ubuntu user: ahjo-state.md (prose context) and
// ahjo-state.json (the statusline's machine-readable twin). Both come from the
// same State, so the two can't drift.
func pushAhjoState(containerName string, st ahjostate.State) error {
	if err := ensureContainerDir(containerName, ahjoStateDirPath); err != nil {
		return err
	}
	if err := pushContainerFile(containerName, ahjostate.RenderMarkdown(st), ahjoStatePath); err != nil {
		return err
	}
	js, err := ahjostate.RenderJSON(st)
	if err != nil {
		return fmt.Errorf("render ahjo-state.json: %w", err)
	}
	return pushContainerFile(containerName, string(js), ahjoStateJSONPath)
}

// ensureContainerDir creates dirPath in the container owned by ubuntu.
// Idempotent; safe even though pushClaudeConfig already creates ~/.claude at
// first launch. Used for both ~/.claude (AHJO.md) and ~/.ahjo (state snapshots).
func ensureContainerDir(containerName, dirPath string) error {
	return incus.ExecAs(containerName, 0, nil, "/",
		"install", "-d", "-m", "0755", "-o", "ubuntu", "-g", "ubuntu", dirPath)
}

// pushContainerFile writes content to a host temp file, pushes it to
// containerPath, and chowns it to uid:gid 1000 (the ubuntu user), mirroring the
// pattern in pushClaudeConfig.
func pushContainerFile(containerName, content, containerPath string) error {
	tmp, err := os.CreateTemp("", "ahjo-push-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := incus.FilePush(containerName, tmp.Name(), containerPath); err != nil {
		return err
	}
	return incus.ExecAs(containerName, 0, nil, "/", "chown", "1000:1000", containerPath)
}
