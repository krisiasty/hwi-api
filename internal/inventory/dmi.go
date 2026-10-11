package inventory

import "strings"

// Identifier is an assessed DMI value. Raw always holds what the firmware
// actually reported, so nothing is lost by the assessment.
type Identifier struct {
	Value  string `json:"value"`
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
	Raw    string `json:"raw"`
}

// placeholders are the values firmware vendors ship when a field was never
// programmed. They are compared lowercased and trimmed.
var placeholders = map[string]struct{}{
	"":                                     {},
	"to be filled by o.e.m.":               {},
	"to be filled by oem":                  {},
	"default string":                       {},
	"not specified":                        {},
	"not applicable":                       {},
	"none":                                 {},
	"unknown":                              {},
	"n/a":                                  {},
	"na":                                   {},
	"empty":                                {},
	"not available":                        {},
	"system serial number":                 {},
	"chassis serial number":                {},
	"base board serial number":             {},
	"0123456789":                           {},
	"1234567890":                           {},
	"123456789":                            {},
	"00000000-0000-0000-0000-000000000000": {},
	"ffffffff-ffff-ffff-ffff-ffffffffffff": {},
}

// NormaliseIdentifier assesses a DMI value.
//
// It exists so that no consumer has to carry its own blocklist, and so
// that two machines both reporting 0123456789 are visibly unusable rather
// than silently identical.
func NormaliseIdentifier(raw string) Identifier {
	trimmed := strings.TrimSpace(raw)
	if _, bad := placeholders[strings.ToLower(trimmed)]; bad {
		return Identifier{Usable: false, Reason: "vendor placeholder or unset", Raw: raw}
	}
	// A value made only of repeated characters is a placeholder in all but
	// name: vendors pad placeholders at length 6+ ("0000000000", "XXXXXXXX").
	// Threshold is 4 to catch every realistic placeholder while leaving short
	// genuine serials like "88" or "AA" alone. When in doubt, accept: wrongly
	// marking a real serial unusable destroys correlation, but wrongly accepting
	// a placeholder merely reaches a consumer already told to treat serials as
	// corroboration only.
	if len(trimmed) > 3 && strings.Count(trimmed, string(trimmed[0])) == len(trimmed) {
		return Identifier{Usable: false, Reason: "repeated single character", Raw: raw}
	}
	return Identifier{Value: trimmed, Usable: true, Raw: raw}
}
