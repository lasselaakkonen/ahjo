// Package lima holds Lima-specific in-guest helpers.
//
// ahjo's in-VM code runs in two contexts: inside a Lima VM driven from a
// macOS host, and on bare-metal Linux. A few code paths only make sense
// under Lima (e.g. ssh-agent forwarding diagnostics, where the agent
// socket comes from the host Mac via lima's SSH master); IsGuest is the
// gate.
package lima

import "os"

const cidataMount = "/mnt/lima-cidata"

// IsGuest reports whether the current process is running inside a Lima VM.
//
// Lima hardcodes /mnt/lima-cidata as the cidata mountpoint in its user-data
// template (see pkg/cidata/cidata.TEMPLATE.d/user-data in lima-vm/lima); it's
// stable across v1/v2 and across vz/qemu drivers. We stat the directory rather
// than reading lima.env so the check works regardless of cidata file perms —
// stat only needs traverse permission on /mnt, not read on the mount contents.
// The path itself is Lima-private, so directory existence has no plausible
// false positives from cloud-init NoCloud, multipass, vagrant, GCE, AWS, etc.
func IsGuest() bool {
	info, err := os.Stat(cidataMount)
	return err == nil && info.IsDir()
}
