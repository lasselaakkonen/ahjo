//go:build !darwin

package paste

import "fmt"

// Run is a stub on non-darwin builds. The paste daemon exists only on
// macOS hosts (where NSPasteboard lives); the in-VM ahjo never invokes it.
func Run() error {
	return fmt.Errorf("ahjo paste-daemon is only supported on macOS")
}
