package inventory

import "testing"

func TestNormaliseIdentifierAcceptsRealValues(t *testing.T) {
	got := NormaliseIdentifier("J8K2M4N6")
	if !got.Usable || got.Value != "J8K2M4N6" {
		t.Errorf("NormaliseIdentifier = %+v, want a usable value", got)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty for a usable value", got.Reason)
	}
}

func TestNormaliseIdentifierRejectsKnownPlaceholders(t *testing.T) {
	testPlaceholders := []string{
		"To Be Filled By O.E.M.",
		"to be filled by o.e.m.",
		"  Default string  ",
		"Not Specified",
		"Not Applicable",
		"None",
		"System Serial Number",
		"Chassis Serial Number",
		"Base Board Serial Number",
		"0123456789",
		"1234567890",
		"123456789",
		"Unknown",
		"Not Available",
		"N/A",
		"",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}

	for _, raw := range testPlaceholders {
		got := NormaliseIdentifier(raw)
		if got.Usable {
			t.Errorf("NormaliseIdentifier(%q) reported usable", raw)
		}
		if got.Value != "" {
			t.Errorf("NormaliseIdentifier(%q).Value = %q, want empty", raw, got.Value)
		}
		if got.Raw != raw {
			t.Errorf("NormaliseIdentifier(%q).Raw = %q, want the original preserved", raw, got.Raw)
		}
		if got.Reason == "" {
			t.Errorf("NormaliseIdentifier(%q) gave no reason", raw)
		}
	}
}

func TestNormaliseIdentifierAccepts2CharRepeated(t *testing.T) {
	// 2-character repeated values like "88" should be usable; they are
	// plausible short serials, not vendor placeholders.
	got := NormaliseIdentifier("88")
	if !got.Usable || got.Value != "88" {
		t.Errorf("NormaliseIdentifier(\"88\") = %+v, want usable", got)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty for usable value", got.Reason)
	}
}

func TestNormaliseIdentifierAccepts3CharRepeated(t *testing.T) {
	// 3-character repeated values like "AAA" should be usable; they are
	// plausible short serials, not vendor placeholders.
	got := NormaliseIdentifier("AAA")
	if !got.Usable || got.Value != "AAA" {
		t.Errorf("NormaliseIdentifier(\"AAA\") = %+v, want usable", got)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty for usable value", got.Reason)
	}
}

func TestNormaliseIdentifierRejects4CharRepeated(t *testing.T) {
	// 4-character and longer repeated values like "0000" are vendor placeholders
	// and should be marked unusable.
	got := NormaliseIdentifier("0000")
	if got.Usable {
		t.Errorf("NormaliseIdentifier(\"0000\") reported usable, want unusable")
	}
	if got.Value != "" {
		t.Errorf("Value = %q, want empty for unusable", got.Value)
	}
	if got.Reason != "repeated single character" {
		t.Errorf("Reason = %q, want \"repeated single character\"", got.Reason)
	}
}
