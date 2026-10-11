package config

import (
	"strings"
	"testing"
)

// TestDecodeRejectsASecondDocument checks that settings in a second YAML
// document are not silently ignored, and neither are its unknown keys.
func TestDecodeRejectsASecondDocument(t *testing.T) {
	for name, in := range map[string]string{
		"a valid setting":     "log:\n  level: warn\n---\napi:\n  auth:\n    enabled: false\n",
		"an unknown key":      "log:\n  level: warn\n---\nnot_a_setting: true\n",
		"a third document":    "log:\n  level: warn\n---\n# empty\n---\nlog:\n  level: debug\n",
		"a bare scalar":       "log:\n  level: warn\n---\nhello\n",
		"after a leading ---": "---\nlog:\n  level: warn\n---\nlog:\n  level: debug\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(in))
			if err == nil {
				t.Fatal("Decode accepted a configuration with more than one document holding content")
			}
			if !strings.Contains(err.Error(), "document") {
				t.Errorf("error = %q, want it to say the file has more than one document", err)
			}
		})
	}
}

// Documents that cannot hold a setting are not a hazard: a leading ---, a
// trailing --- or ... with nothing after it, or a second document with
// only comments.
func TestDecodeAcceptsDocumentsWithoutContent(t *testing.T) {
	for name, in := range map[string]string{
		"leading ---":           "---\nlog:\n  level: warn\n",
		"trailing ---":          "log:\n  level: warn\n---\n",
		"trailing ...":          "log:\n  level: warn\n...\n",
		"comment-only document": "log:\n  level: warn\n---\n# nothing here\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Decode(strings.NewReader(in))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if cfg.Log.Level != "warn" {
				t.Errorf("log.level = %q, want the first document's warn", cfg.Log.Level)
			}
		})
	}
}
