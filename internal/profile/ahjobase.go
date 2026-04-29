// Package profile materializes the embedded ahjo-base profile to disk and
// links it into ~/.coi/profiles/ so `coi build --profile ahjo-base` finds it.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// Materialize writes ~/.ahjo/profiles/ahjo-base/{config.toml,build.sh} from
// the embedded assets and mirrors them into ~/.coi/profiles/ahjo-base/ so
// `coi build --profile ahjo-base` finds the profile. The mirror is a real
// directory containing file-level symlinks back to the canonical copies under
// ~/.ahjo, not a directory-level symlink — see ensureCoiMirror for why.
func Materialize() error {
	cfg, build, err := coi.AhjoBaseAssets()
	if err != nil {
		return fmt.Errorf("read embedded ahjo-base: %w", err)
	}
	dir := paths.ProfilePath(paths.AhjoBaseProfile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := writeFile(filepath.Join(dir, "config.toml"), cfg, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "build.sh"), build, 0o755); err != nil {
		return err
	}
	return ensureCoiMirror()
}

// ensureCoiMirror builds ~/.coi/profiles/<name>/ as a real directory whose
// entries are symlinks to ~/.ahjo/profiles/<name>/<file>. COI's profile
// loader uses fs.DirEntry.IsDir() to filter scan results, and that function
// reports the lstat type — so a symlink-to-a-directory is skipped silently
// and `coi build --profile <name>` errors with "profile not found". File
// symlinks, by contrast, are followed by os.Stat / toml.DecodeFile during
// config load and by resolveAsset when looking up build.sh.
func ensureCoiMirror() error {
	mirror := paths.CoiProfilePath(paths.AhjoBaseProfile)
	canonical := paths.ProfilePath(paths.AhjoBaseProfile)

	// If a stale directory-symlink from an earlier ahjo is here, drop it so
	// MkdirAll below makes a real directory.
	if info, err := os.Lstat(mirror); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(mirror); err != nil {
			return fmt.Errorf("remove stale dir symlink %s: %w", mirror, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s: %w", mirror, err)
	}

	if err := os.MkdirAll(mirror, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mirror, err)
	}
	for _, name := range []string{"config.toml", "build.sh"} {
		if err := refreshFileSymlink(filepath.Join(mirror, name), filepath.Join(canonical, name)); err != nil {
			return err
		}
	}
	return nil
}

func refreshFileSymlink(link, target string) error {
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if cur, lerr := os.Readlink(link); lerr == nil && cur == target {
				return nil
			}
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("remove %s: %w", link, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s: %w", link, err)
	}
	return os.Symlink(target, link)
}

func writeFile(dst string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".write.tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
