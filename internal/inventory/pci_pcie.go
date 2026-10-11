package inventory

import (
	"cmp"
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// PCILink is a PCI Express device's link: the speed and width it trained
// at, and the most it supports. It is reported as the kernel gives it, with
// no verdict: a current width below the maximum is a reliable sign of a
// problem, but a current speed below the maximum is not, since GPUs and
// some NICs lower their link speed when idle.
type PCILink struct {
	// CurrentSpeed and MaxSpeed are the kernel's strings, such as
	// "16.0 GT/s PCIe", or "Unknown" where the kernel cannot tell.
	CurrentSpeed string `json:"current_speed"`
	CurrentWidth int    `json:"current_width"`
	MaxSpeed     string `json:"max_speed"`
	MaxWidth     int    `json:"max_width"`
}

// PCISRIOV is a physical function's SR-IOV capability.
type PCISRIOV struct {
	// TotalVFs is how many virtual functions the device supports.
	TotalVFs int `json:"total_vfs"`
	// NumVFs is how many are configured.
	NumVFs int `json:"num_vfs"`
	// VFs lists the configured virtual functions' addresses, by VF index.
	VFs []string `json:"vfs"`
}

// pciLinkFiles are the files the kernel creates for a PCI Express device's
// link.
var pciLinkFiles = []string{"current_link_speed", "current_link_width", "max_link_speed", "max_link_width"}

// readPCILink reads the link of the device at dir, or nil when it has none.
// A link file that exists but cannot be read or parsed gives "unknown" or
// 0, like any other value.
func readPCILink(fsys fs.FS, dir string) *PCILink {
	if !anyExists(fsys, dir, pciLinkFiles...) {
		return nil
	}
	return &PCILink{
		CurrentSpeed: readTrimmed(fsys, dir+"/current_link_speed"),
		CurrentWidth: readInt(fsys, dir+"/current_link_width"),
		MaxSpeed:     readTrimmed(fsys, dir+"/max_link_speed"),
		MaxWidth:     readInt(fsys, dir+"/max_link_width"),
	}
}

// readPCISRIOV reads the SR-IOV capability of the device at dir, or nil when
// it has none: the kernel creates sriov_totalvfs only on a physical function
// that supports SR-IOV.
func readPCISRIOV(fsys fs.FS, dir string) *PCISRIOV {
	if !anyExists(fsys, dir, "sriov_totalvfs") {
		return nil
	}
	return &PCISRIOV{
		TotalVFs: readInt(fsys, dir+"/sriov_totalvfs"),
		NumVFs:   readInt(fsys, dir+"/sriov_numvfs"),
		VFs:      readVirtualFunctions(fsys, dir),
	}
}

// readVirtualFunctions lists the targets of dir's virtfn<N> links by N. It
// is empty, not nil, so that a physical function with none configured
// reports "vfs": [].
func readVirtualFunctions(fsys fs.FS, dir string) []string {
	type vf struct {
		index   int
		address string
	}
	var vfs []vf
	entries, _ := fs.ReadDir(fsys, dir)
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "virtfn")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		vfs = append(vfs, vf{index: index, address: readLinkBase(fsys, dir+"/"+e.Name())})
	}
	slices.SortFunc(vfs, func(a, b vf) int { return cmp.Compare(a.index, b.index) })
	out := make([]string, 0, len(vfs))
	for _, v := range vfs {
		out = append(out, v.address)
	}
	return out
}

// anyExists reports whether any of names exists in dir. One that exists but
// cannot be examined counts as existing.
func anyExists(fsys fs.FS, dir string, names ...string) bool {
	for _, n := range names {
		if _, err := fs.Stat(fsys, dir+"/"+n); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// readInt reads name as a decimal integer, or 0 when it cannot be read or
// parsed.
func readInt(fsys fs.FS, name string) int {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
