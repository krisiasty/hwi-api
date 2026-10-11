//go:build !linux

package inventory

// PermanentAddresses reports no permanent addresses off Linux, where there is no rtnetlink to ask; the agent runs on Linux only, and this
// keeps it building, and its tests running, elsewhere.
func PermanentAddresses() (map[uint32]string, error) {
	return nil, nil
}
