//go:build !linux

package paths

func MacHostHome() (string, bool) { return "", false }
