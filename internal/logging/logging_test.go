package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewWritesOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("info", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	log.Info("first", "k", 1)
	log.Info("second", "k", 2)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	for _, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("warn", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	log.Info("suppressed")
	if buf.Len() != 0 {
		t.Errorf("info was emitted at warn level: %q", buf.String())
	}

	log.Warn("kept")
	if buf.Len() == 0 {
		t.Error("warn was suppressed at warn level")
	}
}

func TestNewRejectsUnknownLevel(t *testing.T) {
	if _, err := New("chatty", &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an unknown level")
	}
}
