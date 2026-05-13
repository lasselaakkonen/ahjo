//go:build darwin

package paste

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// label is the launchd job label; also used in the plist filename and
	// in `launchctl bootout gui/<uid>/<label>`.
	label = "net.ahjo.paste-daemon"
)

// plistPath returns ~/Library/LaunchAgents/<label>.plist.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func logPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "ahjo-paste-daemon.log"), nil
}

// EnsureRunning is the idempotent host-side entry point called before each
// container-touching subcommand on macOS. Hot path: a 200ms healthz probe
// returns early when the daemon is already up. Cold path: writes the plist
// (or refreshes it when the binary moved), boots out any stale instance,
// then bootstraps under launchd. Every step is best-effort — a failure logs
// to stderr but does NOT block the user's `ahjo claude` invocation.
func EnsureRunning() error {
	if probeHealth(200 * time.Millisecond) {
		return nil
	}
	if err := installAndBootstrap(); err != nil {
		return err
	}
	// Give launchd a moment to spawn the process before re-probing. Up to
	// ~2s — enough cushion that the user doesn't race the daemon on the
	// first invocation post-install.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if probeHealth(150 * time.Millisecond) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("paste-daemon installed but healthz never responded on %s", ListenAddr)
}

// Unload deletes the plist and boots the service out. Called from
// `ahjo nuke` on macOS so a tear-down really tears down. Tolerant — a
// missing plist or already-unloaded service is success.
func Unload() error {
	uid := strconv.Itoa(os.Getuid())
	// bootout is the modern unload; suppress its (very loud) errors when
	// the service isn't loaded, which is the common path after a prior
	// nuke or fresh install.
	bootoutCmd := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label)
	if out, err := bootoutCmd.CombinedOutput(); err != nil {
		low := strings.ToLower(string(out))
		if !(strings.Contains(low, "could not find") ||
			strings.Contains(low, "no such") ||
			strings.Contains(low, "not loaded")) {
			fmt.Fprintf(os.Stderr, "warn: launchctl bootout %s: %v: %s\n", label, err, strings.TrimSpace(string(out)))
		}
	}
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}

// probeHealth performs a fast GET /healthz against the daemon. Returns
// true on a 2xx response within timeout, false on any error or non-2xx.
func probeHealth(timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + ListenAddr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// installAndBootstrap (re)writes the plist if missing or pointing at a
// stale binary path, then bootstraps the service under the user's GUI
// domain. Self-heals when ahjo moved (e.g. brew → /usr/local/bin →
// /opt/homebrew/bin), because each ensure-run compares the current
// os.Executable() with the on-disk plist.
func installAndBootstrap() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve ahjo binary: %w", err)
	}
	exe, _ = filepath.EvalSymlinks(exe)

	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	logFile, err := logPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return fmt.Errorf("mkdir Logs: %w", err)
	}

	want := renderPlist(exe, logFile)
	stale := true
	if cur, err := os.ReadFile(p); err == nil {
		if string(cur) == want {
			stale = false
		}
	}
	if stale {
		if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}

	uid := strconv.Itoa(os.Getuid())
	// Always bootout before bootstrap so a stale binary path can't shadow
	// the freshly-written plist. bootout's failure when not loaded is
	// fine; ignore unless it looks like a real error.
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label).Run()

	bootCmd := exec.Command("launchctl", "bootstrap", "gui/"+uid, p)
	if out, err := bootCmd.CombinedOutput(); err != nil {
		low := strings.ToLower(string(out))
		// "service already loaded" is benign on macOS versions where
		// bootout above was a no-op due to namespacing quirks.
		if strings.Contains(low, "already loaded") || strings.Contains(low, "already exists") {
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("launchctl bootstrap gui/%s %s: exit %d: %s", uid, p, ee.ExitCode(), strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderPlist emits the launchd plist. KeepAlive=true so a crash respawns;
// RunAtLoad=true so a fresh login spins the daemon back up without needing
// an explicit ahjo invocation.
func renderPlist(exe, logFile string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + exe + `</string>
		<string>paste-daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + logFile + `</string>
	<key>StandardErrorPath</key>
	<string>` + logFile + `</string>
</dict>
</plist>
`
}
