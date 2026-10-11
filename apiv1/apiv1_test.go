package apiv1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSubsystemsIsStableAndComplete(t *testing.T) {
	got := Subsystems()
	want := []Subsystem{
		SubsystemCPU, SubsystemMemory, SubsystemTopology, SubsystemBlock,
		SubsystemNetwork, SubsystemLinks, SubsystemPCI, SubsystemSlots, SubsystemChassis,
		SubsystemBIOS, SubsystemBaseboard, SubsystemProduct,
		SubsystemUdevBlock, SubsystemUdevNet, SubsystemUdevPCI, SubsystemUdevNVMe,
		SubsystemUdevSCSI, SubsystemUdevDRM, SubsystemUdevVFIO,
		SubsystemOS, SubsystemVirtualization,
	}
	if len(got) != len(want) {
		t.Fatalf("Subsystems() has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Subsystems()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSubsystemsDoesNotIncludeAll(t *testing.T) {
	for _, s := range Subsystems() {
		if s == SubsystemAll {
			t.Fatal("SubsystemAll must not appear in Subsystems(); it is not a real subsystem")
		}
	}
}

func TestParseSubsystem(t *testing.T) {
	if s, ok := ParseSubsystem("cpu"); !ok || s != SubsystemCPU {
		t.Errorf("ParseSubsystem(cpu) = %q, %v", s, ok)
	}
	if _, ok := ParseSubsystem("all"); ok {
		t.Error("ParseSubsystem(all) succeeded; all is not addressable as a subsystem")
	}
	if _, ok := ParseSubsystem("firmware"); ok {
		t.Error("ParseSubsystem(firmware) succeeded for an unknown name")
	}
	if _, ok := ParseSubsystem("CPU"); ok {
		t.Error("ParseSubsystem is case sensitive; CPU must not resolve")
	}
}

func TestEnvelopeFieldNames(t *testing.T) {
	env := Envelope{
		SchemaVersion: SchemaVersion,
		Subsystem:     SubsystemCPU,
		CollectedAt:   time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
		Complete:      true,
		HostID:        "b41d2f0c9e7a4c1f8d3b6a5e2c9f7d84",
		Data:          json.RawMessage(`{"k":1}`),
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"schema_version", "subsystem", "host_id", "collected_at", "complete", "data"} {
		if _, ok := got[key]; !ok {
			t.Errorf("envelope is missing key %q; got %v", key, got)
		}
	}
	// Absent optional keys must stay absent rather than serialise as null.
	for _, key := range []string{"errors", "disabled"} {
		if _, ok := got[key]; ok {
			t.Errorf("empty %q must be omitted, not emitted", key)
		}
	}
	// The build identity and hostname are deliberately not in the envelope.
	for _, key := range []string{"agent", "host", "hostname"} {
		if _, ok := got[key]; ok {
			t.Errorf("envelope carries removed key %q", key)
		}
	}
	if got["collected_at"] != "2026-09-21T10:00:00Z" {
		t.Errorf("collected_at = %v, want RFC 3339 UTC", got["collected_at"])
	}
}

func TestErrorResponseShape(t *testing.T) {
	b, err := json.Marshal(ErrorResponse{Error: Error{Code: CodeSubsystemDisabled, Message: "subsystem is disabled"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `{"error":{"code":"subsystem_disabled","message":"subsystem is disabled"}}` {
		t.Errorf("error body = %s", b)
	}
}

func TestSubsystemsReturnsACopy(t *testing.T) {
	first := Subsystems()
	first[0] = SubsystemAll // corrupt the returned slice
	second := Subsystems()
	if second[0] != SubsystemCPU {
		t.Errorf("Subsystems()[0] = %q after a caller mutated an earlier result; "+
			"the package's own slice must not be reachable", second[0])
	}
}

// TestEnvelopeKeepsAnEmptyHostID: with no readable machine ID, host_id is
// still present, as "", rather than omitted.
func TestEnvelopeKeepsAnEmptyHostID(t *testing.T) {
	b, err := json.Marshal(Envelope{SchemaVersion: SchemaVersion, Subsystem: SubsystemCPU, Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["host_id"]; !ok || v != "" {
		t.Errorf("host_id = %v (present %v), want present as \"\"", v, ok)
	}
}
