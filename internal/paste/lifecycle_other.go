//go:build !darwin

package paste

// EnsureRunning is a no-op outside macOS — the daemon hosts NSPasteboard
// and only makes sense on the Mac side of the Lima split.
func EnsureRunning() error { return nil }

// Unload is a no-op outside macOS.
func Unload() error { return nil }
