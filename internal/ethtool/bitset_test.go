package ethtool

import (
	"reflect"
	"testing"
)

// testBit is one bit of a verbose bitset fixture.
type testBit struct {
	index uint32
	name  string
	set   bool // encoded as ETHTOOL_A_BITSET_BIT_VALUE; ignored by the kernel's reading of a NOMASK bitset, which lists only set bits
}

// verboseBitset encodes a bitset attribute of type typ the way the kernel replies to a request with header flags 0: the bits listed with their
// index and name, preceded by the NOMASK flag when nomask is set.
func verboseBitset(typ uint16, nomask bool, bits ...testBit) []byte {
	var head []byte
	if nomask {
		head = encodeAttr(bitsetNomask, nil)
	}
	head = append(head, u32Attr(bitsetSize, 128)...)
	var list [][]byte
	for _, b := range bits {
		fields := [][]byte{u32Attr(bitsetBitIndex, b.index), stringAttr(bitsetBitName, b.name)}
		if b.set {
			fields = append(fields, encodeAttr(bitsetBitValue, nil))
		}
		list = append(list, encodeNested(bitsetBitsBit, fields...))
	}
	return encodeNested(typ, head, encodeNested(bitsetBits, list...))
}

// parseOne parses b, which must hold exactly one attribute, and returns it.
func parseOne(t *testing.T, b []byte) attr {
	t.Helper()
	attrs, err := parseAttributes(b)
	if err != nil || len(attrs) != 1 {
		t.Fatalf("parseAttributes = %v, %v; want one attribute", attrs, err)
	}
	return attrs[0]
}

func TestParseBitsetNomask(t *testing.T) {
	// NOMASK lists only set bits, so every listed bit is set whether or not it carries _BIT_VALUE.
	a := parseOne(t, verboseBitset(4, true, testBit{index: 3, name: "rx-checksum", set: true}, testBit{index: 7, name: "rx-gro-hw"}))
	got, err := parseBitset(a)
	if err != nil {
		t.Fatalf("parseBitset: %v", err)
	}
	want := []bit{{index: 3, name: "rx-checksum", set: true}, {index: 7, name: "rx-gro-hw", set: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBitset = %+v, want %+v", got, want)
	}
}

func TestParseBitsetMasked(t *testing.T) {
	a := parseOne(t, verboseBitset(3, false,
		testBit{index: 42, name: "10000baseCR/Full", set: true},
		testBit{index: 10, name: "FIBRE"},
	))
	got, err := parseBitset(a)
	if err != nil {
		t.Fatalf("parseBitset: %v", err)
	}
	want := []bit{{index: 42, name: "10000baseCR/Full", set: true}, {index: 10, name: "FIBRE", set: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBitset = %+v, want %+v (kernel order kept, set only with _BIT_VALUE)", got, want)
	}
}

func TestParseBitsetIgnoresUnknownAttributes(t *testing.T) {
	bitNest := encodeNested(bitsetBitsBit,
		u32Attr(bitsetBitIndex, 5),
		stringAttr(bitsetBitName, "tx-checksum-ip-generic"),
		encodeNested(9, u32Attr(1, 1)), // an attribute a newer kernel might add to a bit
	)
	b := encodeNested(2, encodeAttr(bitsetNomask, nil), u32Attr(99, 1), encodeNested(bitsetBits, bitNest, u32Attr(7, 0)))
	got, err := parseBitset(parseOne(t, b))
	if err != nil {
		t.Fatalf("parseBitset: %v", err)
	}
	want := []bit{{index: 5, name: "tx-checksum-ip-generic", set: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBitset = %+v, want %+v", got, want)
	}
}

func TestParseBitsetEmpty(t *testing.T) {
	// A verbose bitset with nothing to list still carries an empty BITS nest.
	got, err := parseBitset(parseOne(t, verboseBitset(4, true)))
	if err != nil || len(got) != 0 {
		t.Errorf("parseBitset = %+v, %v; want no bits and no error", got, err)
	}
}

func TestParseBitsetRejectsCompact(t *testing.T) {
	compact := encodeNested(4, encodeAttr(bitsetNomask, nil), u32Attr(bitsetSize, 32), encodeAttr(bitsetValue, []byte{0xff, 0, 0, 0}))
	if got, err := parseBitset(parseOne(t, compact)); err == nil {
		t.Errorf("parseBitset(compact) = %+v, nil; want an error, never a silent empty result", got)
	}
	masked := encodeNested(3, u32Attr(bitsetSize, 32), encodeAttr(bitsetValue, []byte{1, 0, 0, 0}), encodeAttr(bitsetMask, []byte{3, 0, 0, 0}))
	if got, err := parseBitset(parseOne(t, masked)); err == nil {
		t.Errorf("parseBitset(compact with mask) = %+v, nil; want an error", got)
	}
}

func TestParseBitsetRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
	}{
		{"bit without an index", encodeNested(4, encodeNested(bitsetBits, encodeNested(bitsetBitsBit, stringAttr(bitsetBitName, "x"))))},
		{"bit index of the wrong width", encodeNested(4, encodeNested(bitsetBits,
			encodeNested(bitsetBitsBit, encodeAttr(bitsetBitIndex, []byte{1}), stringAttr(bitsetBitName, "x"))))},
		{"bits nest that does not parse", encodeNested(4, encodeAttr(bitsetBits|nlaFNested, []byte{0xff, 0xff, 1, 0}))},
		{"bitset that does not parse", encodeAttr(4|nlaFNested, []byte{0xff, 0xff, 1, 0})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseBitset(parseOne(t, tt.b)); err == nil {
				t.Errorf("parseBitset = %+v, nil; want an error", got)
			}
		})
	}
}
