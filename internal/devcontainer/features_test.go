package devcontainer

import (
	"reflect"
	"strings"
	"testing"
)

// SafeRefDir is the readable-ref → filesystem-safe-basename transform
// used by every Apply path (the in-container tmp dir at /tmp/feature-X
// and the host extraction dir under the per-repo-add tmp root). Any
// byte outside [a-zA-Z0-9._-] becomes "-". The transform must be
// deterministic so the same Feature lands at the same path across
// retries.
func TestSafeRefDir(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"OCI ref → slashes and colons become dashes", "ghcr.io/devcontainers/features/node:1", "ghcr.io-devcontainers-features-node-1"},
		{"already-safe ID passes through", "ahjo-runtime", "ahjo-runtime"},
		// SafeRefDir iterates byte-by-byte, so a UTF-8 character (ä = 2
		// bytes) yields two `-` not one. Pinned here so a future
		// rune-aware rewrite is a deliberate, visible change.
		{"unicode and spaces become dashes", "feature with space/ä", "feature-with-space---"},
		{"empty input → empty output", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeRefDir(tc.in); got != tc.want {
				t.Fatalf("SafeRefDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// expandContainerEnv resolves the `${VAR}` interpolation that upstream
// Features (Go, Node, Rust, …) rely on for PATH augmentation. The
// motivating case is the Go Feature's
// `PATH: /usr/local/go/bin:/go/bin:${PATH}` — without expansion, Apply
// passes a literal `${PATH}` to install.sh, whose 00-restore-env.sh
// delta-capture trick then yields a no-op and Go stays off PATH for
// every subsequent login shell.
func TestExpandContainerEnv(t *testing.T) {
	current := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin",
		"HOME": "/home/ubuntu",
	}
	raw := map[string]string{
		"PATH":   "/usr/local/go/bin:/go/bin:${PATH}",
		"GOPATH": "/go",
		"GOROOT": "/usr/local/go",
		// $VAR (no braces) should also expand — os.Expand handles
		// both forms, and a Feature could legally write either.
		"USER_HOME": "$HOME",
		// Unset name expands to "" (shell semantics) rather than
		// staying as a literal `${MISSING}` — that's what would
		// brick later execs.
		"WITH_MISSING": "before:${MISSING}:after",
	}
	got := expandContainerEnv(raw, current)
	want := map[string]string{
		"PATH":         "/usr/local/go/bin:/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin",
		"GOPATH":       "/go",
		"GOROOT":       "/usr/local/go",
		"USER_HOME":    "/home/ubuntu",
		"WITH_MISSING": "before::after",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandContainerEnv mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExpandContainerEnv_EmptyInput(t *testing.T) {
	if got := expandContainerEnv(nil, map[string]string{"PATH": "/x"}); got != nil {
		t.Fatalf("nil raw → nil; got %#v", got)
	}
	if got := expandContainerEnv(map[string]string{}, nil); got != nil {
		t.Fatalf("empty raw → nil; got %#v", got)
	}
}

// ApplyOptionDefaults fills in option keys the user didn't declare,
// using the Feature's metadata defaults. Required so curated Features
// that branch on a default keyword (the motivating case: `git:1`'s
// `version: os-provided`) actually see that keyword at install time.
// User-declared values must always win, even if the metadata default
// disagrees — the user passed the value on purpose.
func TestApplyOptionDefaults(t *testing.T) {
	meta := &Metadata{
		Options: map[string]OptionSpec{
			"version":             {Default: "os-provided"},
			"ppa":                 {Default: true},
			"golangciLintVersion": {Default: "latest"},
			"noDefault":           {Default: nil}, // present but no default — must not appear
		},
	}
	cases := []struct {
		name     string
		userOpts map[string]any
		meta     *Metadata
		want     map[string]any
	}{
		{
			name:     "nil user opts + nil meta → nil",
			userOpts: nil,
			meta:     nil,
			want:     nil,
		},
		{
			name:     "empty user opts → all defaults applied (the git:1 case)",
			userOpts: map[string]any{},
			meta:     meta,
			want: map[string]any{
				"version":             "os-provided",
				"ppa":                 true,
				"golangciLintVersion": "latest",
			},
		},
		{
			name:     "user overrides a default → user wins",
			userOpts: map[string]any{"version": "latest"},
			meta:     meta,
			want: map[string]any{
				"version":             "latest",
				"ppa":                 true,
				"golangciLintVersion": "latest",
			},
		},
		{
			name:     "user passes a key the metadata doesn't know → still kept",
			userOpts: map[string]any{"customKey": "x"},
			meta:     meta,
			want: map[string]any{
				"version":             "os-provided",
				"ppa":                 true,
				"golangciLintVersion": "latest",
				"customKey":           "x",
			},
		},
		{
			name:     "nil-default option key is not synthesized",
			userOpts: nil,
			meta:     meta,
			want: map[string]any{
				"version":             "os-provided",
				"ppa":                 true,
				"golangciLintVersion": "latest",
			},
		},
		{
			name:     "no metadata options → user opts pass through unchanged",
			userOpts: map[string]any{"x": 1},
			meta:     &Metadata{},
			want:     map[string]any{"x": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyOptionDefaults(tc.userOpts, tc.meta)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// rejectDockerFields is the hard-reject gate — it must keep firing on
// `mounts` and `privileged` (Features that genuinely depend on those at
// runtime) and must NOT fire on the warn-and-ignore set (capAdd,
// securityOpt, init, entrypoint) which noteIgnoredDockerFields handles.
//
// The Feature catalog motivating this split: docker-in-docker
// declares both mounts and privileged; nix/kubectl-helm-minikube
// declare mounts; go/rust declare capAdd+securityOpt; desktop-lite
// declares init+entrypoint. We want the first group to error and the
// second to pass through to the warning path.
func TestRejectDockerFields(t *testing.T) {
	tru := true
	cases := []struct {
		name    string
		meta    Metadata
		wantErr string // substring; empty means must succeed
	}{
		{"empty", Metadata{}, ""},
		{"mounts hard-rejects", Metadata{Mounts: []any{map[string]any{"source": "x", "target": "y"}}}, "declares `mounts`"},
		{"privileged hard-rejects", Metadata{Privileged: &tru}, "declares `privileged: true`"},
		{"capAdd no longer rejects", Metadata{CapAdd: []string{"SYS_PTRACE"}}, ""},
		{"securityOpt no longer rejects", Metadata{SecurityOpt: []string{"seccomp=unconfined"}}, ""},
		{"init no longer rejects", Metadata{Init: &tru}, ""},
		{"entrypoint no longer rejects", Metadata{Entrypoint: "/usr/local/share/init.sh"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectDockerFields(&tc.meta, "ghcr.io/x/y:1")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// noteIgnoredDockerFields emits one note per declared field. Known
// values (SYS_PTRACE, seccomp=unconfined, label=disable) get
// value-specific text that names the Docker concept and explains why
// it's a no-op on ahjo; unknown values fall through to a generic note
// that still tells the user the field was dropped. The test pins the
// well-known cases by substring so the message can evolve without
// becoming a string-equality straitjacket.
func TestNoteIgnoredDockerFields(t *testing.T) {
	tru := true
	cases := []struct {
		name    string
		meta    Metadata
		wantLen int
		// substrings each note must contain, in order
		wantSubs [][]string
	}{
		{
			name:    "no fields declared → no notes",
			meta:    Metadata{},
			wantLen: 0,
		},
		{
			name:     "init: true → systemd no-op note",
			meta:     Metadata{Init: &tru},
			wantLen:  1,
			wantSubs: [][]string{{"init: true", "systemd as PID 1"}},
		},
		{
			name:     "SYS_PTRACE → debugger-specific note",
			meta:     Metadata{CapAdd: []string{"SYS_PTRACE"}},
			wantLen:  1,
			wantSubs: [][]string{{"SYS_PTRACE", "delve", "in-container ptrace"}},
		},
		{
			name:     "unknown cap → generic capability note",
			meta:     Metadata{CapAdd: []string{"NET_ADMIN"}},
			wantLen:  1,
			wantSubs: [][]string{{"NET_ADMIN", "EPERM"}},
		},
		{
			name:     "seccomp=unconfined → seccomp note",
			meta:     Metadata{SecurityOpt: []string{"seccomp=unconfined"}},
			wantLen:  1,
			wantSubs: [][]string{{"seccomp=unconfined", "Incus profile"}},
		},
		{
			name:     "label=disable → SELinux note",
			meta:     Metadata{SecurityOpt: []string{"label=disable"}},
			wantLen:  1,
			wantSubs: [][]string{{"label=disable", "SELinux"}},
		},
		{
			name:     "entrypoint → runtime-bootstrap warning",
			meta:     Metadata{Entrypoint: "/usr/local/share/docker-init.sh"},
			wantLen:  1,
			wantSubs: [][]string{{"/usr/local/share/docker-init.sh", "runtime breakage"}},
		},
		{
			name: "go-style trio → three notes in field order",
			meta: Metadata{
				Init:        &tru,
				CapAdd:      []string{"SYS_PTRACE"},
				SecurityOpt: []string{"seccomp=unconfined"},
			},
			wantLen: 3,
			wantSubs: [][]string{
				{"init: true"},
				{"SYS_PTRACE"},
				{"seccomp=unconfined"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := noteIgnoredDockerFields(&tc.meta, "ghcr.io/devcontainers/features/example:1")
			if len(got) != tc.wantLen {
				t.Fatalf("note count: want %d, got %d (%v)", tc.wantLen, len(got), got)
			}
			for i, subs := range tc.wantSubs {
				for _, sub := range subs {
					if !strings.Contains(got[i], sub) {
						t.Fatalf("note %d missing %q: %s", i, sub, got[i])
					}
				}
				// Every note must name the Feature so the user can tell
				// which Feature in a multi-Feature install is being warned about.
				if !strings.Contains(got[i], "ghcr.io/devcontainers/features/example:1") {
					t.Fatalf("note %d missing Feature ID: %s", i, got[i])
				}
			}
		})
	}
}
