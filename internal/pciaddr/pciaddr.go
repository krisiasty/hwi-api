// Package pciaddr recognises PCI addresses as the kernel writes them. The
// privileged helper checks the addresses it is asked to read VPD for with
// it, and the daemon's collectors pick devices out of sysfs paths with it,
// so the two can never disagree about what an address is.
package pciaddr

import "regexp"

var pattern = regexp.MustCompile(`^[0-9a-f]{4,}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// Valid reports whether s is a PCI address, domain:bus:device.function, in
// lowercase hex. A domain is usually four hex digits, but an Intel VMD
// domain has five, such as 10000.
func Valid(s string) bool { return pattern.MatchString(s) }
