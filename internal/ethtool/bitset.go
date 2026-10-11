package ethtool

import "fmt"

// Bitset attributes, from include/uapi/linux/ethtool_netlink_generated.h.
const (
	bitsetNomask = 1 // ETHTOOL_A_BITSET_NOMASK (flag): only set bits are listed
	bitsetSize   = 2 // ETHTOOL_A_BITSET_SIZE (u32)
	bitsetBits   = 3 // ETHTOOL_A_BITSET_BITS (nest): the verbose form's bit list
	bitsetValue  = 4 // ETHTOOL_A_BITSET_VALUE (binary): the compact form's values
	bitsetMask   = 5 // ETHTOOL_A_BITSET_MASK (binary): the compact form's mask

	bitsetBitsBit = 1 // ETHTOOL_A_BITSET_BITS_BIT (nest): one listed bit

	bitsetBitIndex = 1 // ETHTOOL_A_BITSET_BIT_INDEX (u32)
	bitsetBitName  = 2 // ETHTOOL_A_BITSET_BIT_NAME (string)
	bitsetBitValue = 3 // ETHTOOL_A_BITSET_BIT_VALUE (flag): the bit is set
)

// bit is one bit of a decoded verbose bitset.
type bit struct {
	index uint32
	name  string
	set   bool
}

// parseBitset decodes a verbose bitset: the listed bits in kernel order, each with whether its value is set. In a NOMASK bitset the kernel lists
// only set bits, so every listed bit is set; otherwise it lists the mask's bits and marks the set ones with _BIT_VALUE.
//
// Requests ask for verbose bitsets (header flags 0), so a compact one is an error: reading it as verbose would report no bits at all.
func parseBitset(a attr) ([]bit, error) {
	attrs, err := parseAttributes(a.data)
	if err != nil {
		return nil, fmt.Errorf("bitset %d: %w", a.typ, err)
	}
	var nomask, compact bool
	var list *attr
	for i := range attrs {
		switch attrs[i].typ {
		case bitsetNomask:
			nomask = true
		case bitsetBits:
			list = &attrs[i]
		case bitsetValue, bitsetMask:
			compact = true
		}
	}
	if list == nil {
		if compact {
			return nil, fmt.Errorf("bitset %d: compact bitsets are not requested", a.typ)
		}
		return nil, nil
	}

	entries, err := parseAttributes(list.data)
	if err != nil {
		return nil, fmt.Errorf("bitset %d bits: %w", a.typ, err)
	}
	bits := make([]bit, 0, len(entries))
	for _, e := range entries {
		if e.typ != bitsetBitsBit {
			continue
		}
		b, err := parseBit(e, nomask)
		if err != nil {
			return nil, fmt.Errorf("bitset %d: %w", a.typ, err)
		}
		bits = append(bits, b)
	}
	return bits, nil
}

// parseBit decodes one ETHTOOL_A_BITSET_BITS_BIT nest; attributes it does not know are ignored, as a newer kernel may add some.
func parseBit(e attr, nomask bool) (bit, error) {
	fields, err := parseAttributes(e.data)
	if err != nil {
		return bit{}, fmt.Errorf("bit: %w", err)
	}
	b := bit{set: nomask}
	hasIndex := false
	for _, f := range fields {
		switch f.typ {
		case bitsetBitIndex:
			if b.index, err = f.u32(); err != nil {
				return bit{}, fmt.Errorf("bit index: %w", err)
			}
			hasIndex = true
		case bitsetBitName:
			b.name = f.str()
		case bitsetBitValue:
			b.set = true
		}
	}
	if !hasIndex {
		return bit{}, fmt.Errorf("bit %q has no index", b.name)
	}
	return b, nil
}
