//go:build darwin

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const releaseBaseURL = "https://github.com/lasselaakkonen/ahjo/releases/download"

// resolveLinuxBinaryLocal returns a local path to ahjo-linux-<arch> without
// touching the network. Empty string means "not found locally".
func resolveLinuxBinaryLocal(version, arch string) string {
	name := "ahjo-linux-" + arch
	if v := os.Getenv("AHJO_LINUX_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if exe, err := os.Executable(); err == nil {
		// EvalSymlinks so that `ln -s $PWD/ahjo /usr/local/bin/ahjo` (the
		// recommended source-install) resolves back to the repo dir, where
		// `make build` drops the matching ahjo-linux-<arch> into ./dist/.
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		d := filepath.Dir(exe)
		for _, p := range []string{
			filepath.Join(d, name),
			filepath.Join(d, "dist", name),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		cached := filepath.Join(home, ".ahjo", "cache", fmt.Sprintf("%s-%s", name, version))
		if _, err := os.Stat(cached); err == nil {
			return cached
		}
	}
	return ""
}

// downloadLinuxBinary fetches ahjo-linux-<arch> from the GitHub release that
// matches version, verifies it against the release's SHA256SUMS, caches it at
// ~/.ahjo/cache/ahjo-linux-<arch>-<version>, and returns that path. Uses the
// curl/shasum already on every macOS — keeps net/http+TLS out of the binary.
func downloadLinuxBinary(out io.Writer, version, arch string) (string, error) {
	if version == "dev" || version == "" {
		return "", fmt.Errorf(
			"no Linux binary found locally and a `dev` build cannot fetch a release.\n"+
				"  - run `make build` from the repo (drops dist/ahjo-linux-%s next to ./ahjo), or\n"+
				"  - set AHJO_LINUX_BIN=/path/to/ahjo-linux-%s", arch, arch)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(home, ".ahjo", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	name := "ahjo-linux-" + arch
	cached := filepath.Join(cacheDir, fmt.Sprintf("%s-%s", name, version))
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}

	want, err := fetchExpectedSHA(out, version, name)
	if err != nil {
		return "", fmt.Errorf("fetch SHA256SUMS: %w", err)
	}

	url := fmt.Sprintf("%s/%s/%s", releaseBaseURL, version, name)
	tmp := cached + ".part"
	fmt.Fprintf(out, "  → curl -fsSL %s\n", url)
	if err := curlDownload(url, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	got, err := shasum256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, cached); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "  → cached at %s\n", cached)
	return cached, nil
}

func fetchExpectedSHA(out io.Writer, version, name string) (string, error) {
	url := fmt.Sprintf("%s/%s/SHA256SUMS", releaseBaseURL, version)
	fmt.Fprintf(out, "  → curl -fsSL %s\n", url)
	body, err := exec.Command("curl", "-fsSL", url).Output()
	if err != nil {
		return "", fmt.Errorf("curl %s: %w", url, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no entry for %s in SHA256SUMS at %s", name, url)
}

func curlDownload(url, dest string) error {
	cmd := exec.Command("curl", "-fsSL", "-o", dest, url)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// shasum256 returns hex sha256 of path using /usr/bin/shasum (preinstalled on
// every macOS). Avoids pulling crypto/sha256 into the binary.
func shasum256(path string) (string, error) {
	out, err := exec.Command("shasum", "-a", "256", path).Output()
	if err != nil {
		return "", fmt.Errorf("shasum %s: %w", path, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 1 {
		return "", fmt.Errorf("shasum: unexpected output %q", string(out))
	}
	return f[0], nil
}
