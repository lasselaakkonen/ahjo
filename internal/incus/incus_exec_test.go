package incus

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
)

// This file exercises the execCommand seam: every wrapper's argv construction,
// the JSON/text parsing helpers, and the "already exists"/"not found"
// tolerance branches — all without a real `incus` binary. The fake records the
// argv of each call and replays canned stdout/stderr/exit codes via a helper
// subprocess (the standard os/exec test pattern).

// fakeRun is one canned `incus` invocation result.
type fakeRun struct {
	stdout string
	stderr string
	exit   int
}

// fakeExec records the argv of each execCommand call and replays the canned
// runs in order. Calls beyond len(runs) get a zero-value (empty, exit 0) run.
type fakeExec struct {
	runs  []fakeRun
	n     int
	Calls [][]string // each entry is {"incus", arg, ...}
}

func (fe *fakeExec) command(name string, args ...string) *exec.Cmd {
	fe.Calls = append(fe.Calls, append([]string{name}, args...))
	var r fakeRun
	if fe.n < len(fe.runs) {
		r = fe.runs[fe.n]
	}
	fe.n++
	return helperCommand(r)
}

// withFakeExec swaps execCommand for a recording fake and restores the real
// seam on cleanup. Pass one fakeRun per expected invocation.
func withFakeExec(t *testing.T, runs ...fakeRun) *fakeExec {
	t.Helper()
	fe := &fakeExec{runs: runs}
	orig := execCommand
	execCommand = fe.command
	t.Cleanup(func() { execCommand = orig })
	return fe
}

// helperCommand builds a *exec.Cmd that re-runs this test binary as
// TestHelperProcess, which echoes the canned stdout/stderr and exits with the
// canned code — standing in for a real `incus` invocation.
func helperCommand(r fakeRun) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"HELPER_STDOUT="+r.stdout,
		"HELPER_STDERR="+r.stderr,
		"HELPER_EXIT="+strconv.Itoa(r.exit),
	)
	return cmd
}

// TestHelperProcess is not a real test: it's the child the fake seam runs in
// place of `incus`. It is inert unless GO_WANT_HELPER_PROCESS=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	if s := os.Getenv("HELPER_STDOUT"); s != "" {
		fmt.Fprint(os.Stdout, s)
	}
	if s := os.Getenv("HELPER_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

// TestArgvConstruction is the core table: each wrapper must build exactly the
// argv it always has. A drift here is a behavior change in what ahjo asks
// incus to do.
func TestArgvConstruction(t *testing.T) {
	tests := []struct {
		name string
		call func()
		want []string
	}{
		{
			name: "Exec",
			call: func() { _, _ = Exec("c1", "echo", "hi") },
			want: []string{"incus", "exec", "c1", "--", "echo", "hi"},
		},
		{
			name: "AddProxyDevice",
			call: func() { _ = AddProxyDevice("c1", "dev", "tcp:127.0.0.1:80", "tcp:10.0.0.1:80") },
			want: []string{"incus", "config", "device", "add", "c1", "dev", "proxy", "listen=tcp:127.0.0.1:80", "connect=tcp:10.0.0.1:80"},
		},
		{
			name: "AddDiskDevice readonly",
			call: func() { _ = AddDiskDevice("c1", "d", "/src", "/dst", true) },
			want: []string{"incus", "config", "device", "add", "c1", "d", "disk", "source=/src", "path=/dst", "readonly=true"},
		},
		{
			name: "AddDiskDevice writable",
			call: func() { _ = AddDiskDevice("c1", "d", "/src", "/dst", false) },
			want: []string{"incus", "config", "device", "add", "c1", "d", "disk", "source=/src", "path=/dst"},
		},
		{
			name: "AddUnixDevice",
			call: func() { _ = AddUnixDevice("c1", "loop0", "unix-block", "/dev/loop0") },
			want: []string{"incus", "config", "device", "add", "c1", "loop0", "unix-block", "source=/dev/loop0"},
		},
		{
			name: "ConfigSet",
			call: func() { _ = ConfigSet("c1", "security.nesting", "true") },
			want: []string{"incus", "config", "set", "c1", "security.nesting=true"},
		},
		{
			name: "ConfigGet",
			call: func() { _, _ = ConfigGet("c1", "image.os") },
			want: []string{"incus", "config", "get", "c1", "image.os"},
		},
		{
			name: "ConfigDeviceSet",
			call: func() { _ = ConfigDeviceSet("c1", "eth0", "limits.ingress", "100Mbit") },
			want: []string{"incus", "config", "device", "set", "c1", "eth0", "limits.ingress=100Mbit"},
		},
		{
			name: "Stop",
			call: func() { _ = Stop("c1") },
			want: []string{"incus", "stop", "c1"},
		},
		{
			name: "Start",
			call: func() { _ = Start("c1") },
			want: []string{"incus", "start", "c1"},
		},
		{
			name: "LaunchStopped",
			call: func() { _ = LaunchStopped("img", "c1") },
			want: []string{"incus", "init", "img", "c1"},
		},
		{
			name: "Launch",
			call: func() { _ = Launch("img", "c1") },
			want: []string{"incus", "launch", "img", "c1"},
		},
		{
			name: "CopyContainer",
			call: func() { _ = CopyContainer("src", "dst") },
			want: []string{"incus", "copy", "--stateless", "src", "dst"},
		},
		{
			name: "ContainerDeleteForce",
			call: func() { _ = ContainerDeleteForce("c1") },
			want: []string{"incus", "delete", "--force", "c1"},
		},
		{
			name: "DeleteImageAlias",
			call: func() { _ = DeleteImageAlias("ahjo-base") },
			want: []string{"incus", "image", "delete", "ahjo-base"},
		},
		{
			name: "ImageCopyRemote",
			call: func() { _ = ImageCopyRemote("images:ubuntu/24.04", "ahjo-base") },
			want: []string{"incus", "image", "copy", "images:ubuntu/24.04", "local:", "--alias", "ahjo-base"},
		},
		{
			name: "FilePushRecursive",
			call: func() { _ = FilePushRecursive("c1", "/host/dir", "/etc/x") },
			want: []string{"incus", "file", "push", "--recursive", "/host/dir", "c1/etc/x"},
		},
		{
			name: "RemoveDevice",
			call: func() { _ = RemoveDevice("c1", "dev") },
			want: []string{"incus", "config", "device", "remove", "c1", "dev"},
		},
		{
			name: "SystemctlDaemonReload",
			call: func() { _ = SystemctlDaemonReload("c1") },
			want: []string{"incus", "exec", "c1", "--", "systemctl", "daemon-reload"},
		},
		{
			name: "SystemctlEnableNow",
			call: func() { _ = SystemctlEnableNow("c1", "foo.service") },
			want: []string{"incus", "exec", "c1", "--", "systemctl", "enable", "--now", "foo.service"},
		},
		{
			name: "SystemctlDisableNow",
			call: func() { _ = SystemctlDisableNow("c1", "foo.service") },
			want: []string{"incus", "exec", "c1", "--", "systemctl", "disable", "--now", "foo.service"},
		},
		{
			name: "SystemctlStop",
			call: func() { _ = SystemctlStop("c1", "foo.service") },
			want: []string{"incus", "exec", "c1", "--", "systemctl", "stop", "foo.service"},
		},
		{
			// ExecAs threads --user, --cwd, sorted --env pairs, then -- argv.
			name: "ExecAs with env+cwd",
			call: func() { _ = ExecAs("c1", 1000, map[string]string{"B": "2", "A": "1"}, "/work", "ls", "-la") },
			want: []string{"incus", "exec", "c1", "--user", "1000", "--cwd", "/work", "--env", "A=1", "--env", "B=2", "--", "ls", "-la"},
		},
		{
			name: "ExecAs no env no cwd",
			call: func() { _ = ExecAs("c1", 0, nil, "", "true") },
			want: []string{"incus", "exec", "c1", "--user", "0", "--", "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := withFakeExec(t, fakeRun{}) // exit 0, empty output
			tt.call()
			if len(fe.Calls) != 1 {
				t.Fatalf("expected 1 incus call, got %d: %v", len(fe.Calls), fe.Calls)
			}
			if !reflect.DeepEqual(fe.Calls[0], tt.want) {
				t.Fatalf("argv mismatch:\n got %q\nwant %q", fe.Calls[0], tt.want)
			}
		})
	}
}

// TestPublishArgv guards the two-call shape: Publish force-clears the alias
// before publishing, because `incus publish` errors on a pre-existing alias.
func TestPublishArgv(t *testing.T) {
	fe := withFakeExec(t, fakeRun{}, fakeRun{})
	if err := Publish("c1", "ahjo-base"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := [][]string{
		{"incus", "image", "delete", "ahjo-base"},
		{"incus", "publish", "c1", "--alias", "ahjo-base"},
	}
	if !reflect.DeepEqual(fe.Calls, want) {
		t.Fatalf("Publish calls:\n got %q\nwant %q", fe.Calls, want)
	}
}

func TestContainerExists(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   bool
	}{
		{"present", `[{"name":"c1"}]`, true},
		{"absent", `[{"name":"other"}]`, false},
		{"empty", `[]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeExec(t, fakeRun{stdout: tt.stdout})
			got, err := ContainerExists("c1")
			if err != nil {
				t.Fatalf("ContainerExists: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ContainerExists=%v want %v", got, tt.want)
			}
		})
	}
}

// TestContainersWithPrefix pins the "-" boundary: a name that merely shares a
// fragment ("ahjo-foobar") must not match prefix "ahjo-foo".
func TestContainersWithPrefix(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: `[{"name":"ahjo-foobar"},{"name":"ahjo-foo-1"},{"name":"ahjo-foo"},{"name":"unrelated"}]`})
	got, err := ContainersWithPrefix("ahjo-foo")
	if err != nil {
		t.Fatalf("ContainersWithPrefix: %v", err)
	}
	want := []string{"ahjo-foo", "ahjo-foo-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ContainersWithPrefix=%q want %q", got, want)
	}
}

func TestContainerStatus(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: `[{"name":"c1","status":"Running"}]`})
	got, err := ContainerStatus("c1")
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if got != "Running" {
		t.Fatalf("ContainerStatus=%q want Running", got)
	}
}

func TestImageAliasExists(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: `[{"name":"ahjo-base"},{"name":"other"}]`})
	got, err := ImageAliasExists("ahjo-base")
	if err != nil {
		t.Fatalf("ImageAliasExists: %v", err)
	}
	if !got {
		t.Fatalf("ImageAliasExists=false want true")
	}
}

func TestConfigGetTrims(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: "btrfs\n"})
	got, err := ConfigGet("c1", "k")
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "btrfs" {
		t.Fatalf("ConfigGet=%q want %q", got, "btrfs")
	}
}

func TestHasDevice(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: "ssh-agent\nroot\n"})
	got, err := HasDevice("c1", "ssh-agent")
	if err != nil {
		t.Fatalf("HasDevice: %v", err)
	}
	if !got {
		t.Fatalf("HasDevice=false want true")
	}
}

func TestStoragePoolDriver(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"default pool wins", `[{"name":"other","driver":"zfs"},{"name":"default","driver":"btrfs"}]`, "btrfs"},
		{"fallback to first", `[{"name":"other","driver":"zfs"}]`, "zfs"},
		{"none", `[]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeExec(t, fakeRun{stdout: tt.stdout})
			got, err := StoragePoolDriver()
			if err != nil {
				t.Fatalf("StoragePoolDriver: %v", err)
			}
			if got != tt.want {
				t.Fatalf("StoragePoolDriver=%q want %q", got, tt.want)
			}
		})
	}
}

// TestListProxyDevices checks the hand-rolled `config device show` parser:
// only type:proxy devices come back, carrying their listen/connect strings.
func TestListProxyDevices(t *testing.T) {
	show := "ahjo-forward-3000:\n" +
		"  type: proxy\n" +
		"  bind: container\n" +
		"  connect: tcp:10.0.0.1:8000\n" +
		"  listen: tcp:127.0.0.1:3000\n" +
		"root:\n" +
		"  path: /\n" +
		"  type: disk\n"
	withFakeExec(t, fakeRun{stdout: show})
	got, err := ListProxyDevices("c1")
	if err != nil {
		t.Fatalf("ListProxyDevices: %v", err)
	}
	want := []ProxyDevice{{Name: "ahjo-forward-3000", Listen: "tcp:127.0.0.1:3000", Connect: "tcp:10.0.0.1:8000"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListProxyDevices:\n got %+v\nwant %+v", got, want)
	}
}

// TestToleranceBranches: each of these wrappers swallows a specific class of
// non-zero exit (the resource is already gone / already there) and returns nil
// so callers can use them as idempotent steps.
func TestToleranceBranches(t *testing.T) {
	tests := []struct {
		name string
		run  fakeRun
		call func() error
	}{
		{"Stop tolerates not-running", fakeRun{stderr: "Error: Instance is not running", exit: 1}, func() error { return Stop("c1") }},
		{"Start tolerates already-running", fakeRun{stderr: "Error: The instance is already running", exit: 1}, func() error { return Start("c1") }},
		{"DeleteImageAlias tolerates not-found", fakeRun{stderr: "Error: Image not found", exit: 1}, func() error { return DeleteImageAlias("x") }},
		{"ContainerDeleteForce tolerates not-found", fakeRun{stderr: "Error: Instance not found", exit: 1}, func() error { return ContainerDeleteForce("c1") }},
		{"AddProxyDevice tolerates already-exists", fakeRun{stderr: "Error: device already exists", exit: 1}, func() error { return AddProxyDevice("c1", "d", "l", "c") }},
		{"RemoveDevice tolerates not-found", fakeRun{stderr: "Error: device doesn't exist", exit: 1}, func() error { return RemoveDevice("c1", "d") }},
		{"ImageCopyRemote tolerates already-exists", fakeRun{stderr: "Error: Alias already exists", exit: 1}, func() error { return ImageCopyRemote("r", "a") }},
		{"SystemctlDisableNow tolerates not-loaded", fakeRun{stdout: "Failed: Unit foo.service not loaded.", exit: 1}, func() error { return SystemctlDisableNow("c1", "foo.service") }},
		{"SystemctlStop tolerates not-loaded", fakeRun{stdout: "Failed: Unit foo.service not loaded.", exit: 1}, func() error { return SystemctlStop("c1", "foo.service") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeExec(t, tt.run)
			if err := tt.call(); err != nil {
				t.Fatalf("expected tolerated nil, got %v", err)
			}
		})
	}
}

// TestErrorSurfacedOnRealFailure: an unrelated non-zero exit must NOT be
// swallowed — it surfaces with the exit code.
func TestErrorSurfacedOnRealFailure(t *testing.T) {
	withFakeExec(t, fakeRun{stderr: "Error: permission denied", exit: 1})
	err := AddProxyDevice("c1", "d", "l", "c")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSystemctlIsActive(t *testing.T) {
	tests := []struct {
		name    string
		exit    int
		want    bool
		wantErr bool
	}{
		{"active", 0, true, false},
		{"inactive (exit 3)", 3, false, false},
		{"no-such-unit (exit 4)", 4, false, false},
		{"hard error (exit 5)", 5, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeExec(t, fakeRun{exit: tt.exit})
			got, err := SystemctlIsActive("c1", "foo.service")
			if tt.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("SystemctlIsActive=%v want %v", got, tt.want)
			}
		})
	}
}
