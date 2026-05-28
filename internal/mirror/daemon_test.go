package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCopyFile_SymlinkContainment verifies the mirror refuses to recreate a
// repo symlink whose target would escape the mirror root, while still copying
// benign in-tree symlinks. The mirror writes to a host-side dir, so an
// out-of-root target would plant a host symlink pointing at an arbitrary file.
func TestCopyFile_SymlinkContainment(t *testing.T) {
	src := t.TempDir()
	dstRoot := t.TempDir()

	cases := []struct {
		name       string
		target     string
		wantUnsafe bool
	}{
		{"absolute escape", "/etc/passwd", true},
		{"parent escape", "../../../../etc/passwd", true},
		{"in-tree relative", "sibling", false},
		{"in-tree subdir", "nested/leaf", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(src, "link")
			_ = os.Remove(link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatalf("make src symlink: %v", err)
			}
			dst := filepath.Join(dstRoot, "link")
			_ = os.Remove(dst)

			err := CopyFile(link, dst, dstRoot, false)
			if tc.wantUnsafe {
				if !errors.Is(err, ErrUnsafeSymlink) {
					t.Fatalf("target %q: want ErrUnsafeSymlink, got %v", tc.target, err)
				}
				if _, lerr := os.Lstat(dst); lerr == nil {
					t.Fatalf("target %q: unsafe symlink was planted at dst", tc.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("target %q: unexpected error %v", tc.target, err)
			}
			got, lerr := os.Readlink(dst)
			if lerr != nil || got != tc.target {
				t.Fatalf("target %q: dst link = %q, %v", tc.target, got, lerr)
			}
		})
	}
}
