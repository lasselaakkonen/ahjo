// Package lima holds Lima-specific in-guest helpers.
//
// ahjo's in-VM init runs in two contexts: inside a Lima VM driven from a
// macOS host, and on bare-metal Linux. A few steps (auto-disabling ufw,
// forcing COI's open networking mode) only make sense under Lima where
// macOS/vzNAT already firewalls the VM and there's no useful firewalld
// path. Other steps are identical. IsGuest is the gate.
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
