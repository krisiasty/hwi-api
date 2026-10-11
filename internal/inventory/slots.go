package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
)

// Slots is the slots payload: the expansion slots the firmware lists in
// SMBIOS, empty ones included, which a list of PCI devices cannot show.
type Slots struct {
	// Slots lists the slots in the firmware's order. It is null, with
	// SlotsError saying why, when the SMBIOS table cannot be read or
	// decoded, and empty for a table without slots, as in most VMs.
	Slots      []SystemSlot `json:"slots"`
	SlotsError string       `json:"slots_error,omitempty"`
}

// NewSlotsProber reports the system slots from the SMBIOS table, read
// through privileged, the helper's file system. Like the memory modules,
// a table that cannot be had is reported in SlotsError, and logged when it
// exists but cannot be read or decoded; the probe itself never fails.
func NewSlotsProber(privileged fs.FS, log *slog.Logger) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		table, err := readSMBIOSTable(privileged)
		if err == nil {
			var slots []SystemSlot
			if slots, err = systemSlots(table); err == nil {
				return &Slots{Slots: slots}, nil
			}
			err = fmt.Errorf("decoding the SMBIOS table: %w", err)
		}
		if !errors.Is(err, errNoSMBIOSTable) {
			log.Warn("system slots not reported", "error", err)
		}
		return &Slots{SlotsError: err.Error()}, nil
	})
}

// readSMBIOSTable reads the SMBIOS table through privileged. The expected
// cases of no table, no helper or a host without one, match
// errNoSMBIOSTable.
func readSMBIOSTable(privileged fs.FS) ([]byte, error) {
	if privileged == nil {
		return nil, fmt.Errorf("%w: the privileged helper is not configured", errNoSMBIOSTable)
	}
	table, err := fs.ReadFile(privileged, SMBIOSTableFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: the host has none", errNoSMBIOSTable)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the SMBIOS table through the helper: %w", err)
	}
	return table, nil
}
