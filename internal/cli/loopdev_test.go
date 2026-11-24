package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoopBound(t *testing.T) {
	root := t.TempDir()
	old := loopSysfsRoot
	loopSysfsRoot = root
	t.Cleanup(func() { loopSysfsRoot = old })

	// loop0 bound, loop1 present but unbound, loop2 absent.
	if err := os.MkdirAll(filepath.Join(root, "loop0", "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loop0", "loop", "backing_file"), []byte("/x.img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "loop1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !loopBound(0) {
		t.Error("loop0 should be bound")
	}
	if loopBound(1) {
		t.Error("loop1 should be unbound")
	}
	if loopBound(2) {
		t.Error("loop2 (absent) should be unbound")
	}
	// Non-ENOENT error fails closed: a regular file where a dir is expected.
	if err := os.WriteFile(filepath.Join(root, "loop3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !loopBound(3) {
		t.Error("stat error other than not-exist must count as bound")
	}
}

func TestPlanLoopDevices(t *testing.T) {
	set := func(xs ...int) func(int) bool {
		m := map[int]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return func(i int) bool { return m[i] }
	}
	tests := []struct {
		name          string
		have          map[int]bool
		exists, bound func(int) bool
		remove, add   []int
	}{
		{"bound and configured is removed", map[int]bool{0: true}, set(0), set(0), []int{0}, nil},
		{"missing node and configured is removed", map[int]bool{1: true}, set(), set(), []int{1}, nil},
		{"free and unconfigured is added", nil, set(2), set(), nil, []int{2}},
		{"bound and unconfigured is skipped", nil, set(3), set(3), nil, nil},
		{"free and configured is kept", map[int]bool{4: true}, set(4), set(), nil, nil},
		{"mixed", map[int]bool{0: true, 1: true}, set(0, 1, 2, 3), set(0, 3), []int{0}, []int{2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remove, add := planLoopDevices(tt.have, tt.exists, tt.bound)
			if !reflect.DeepEqual(remove, tt.remove) || !reflect.DeepEqual(add, tt.add) {
				t.Fatalf("remove=%v add=%v want remove=%v add=%v", remove, add, tt.remove, tt.add)
			}
		})
	}
}
