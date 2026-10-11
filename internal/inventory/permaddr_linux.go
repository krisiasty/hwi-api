//go:build linux

package inventory

import "syscall"

// PermanentAddresses asks the kernel for every interface's permanent (burned-in) address, by interface index, in one rtnetlink link dump:
// IFLA_PERM_ADDRESS, which Linux 5.6 and later report. An interface the kernel reports none for, such as a virtual one, is absent. The dump
// is bounded: see boundedPermanentAddresses. Its errors start with "permanent addresses:".
func PermanentAddresses() (map[uint32]string, error) {
	return boundedPermanentAddresses(func() ([]byte, error) {
		return syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	})
}
