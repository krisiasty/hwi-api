package pciaddr

import "testing"

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"0000:3b:00.0":   true,
		"10000:01:00.0":  true, // an Intel VMD domain
		"pci0000:3a":     false,
		"0000:3b:00.8":   false,
		"0000:3b:00":     false,
		"0000:3B:00.0":   false, // the kernel writes lowercase hex
		"0000:3b:00.0\n": false, // as read from a sysfs file, untrimmed
		"":               false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
