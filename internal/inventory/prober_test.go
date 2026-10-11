package inventory

import (
	"context"
	"errors"
	"testing"
)

func TestProberFuncAdaptsAFunction(t *testing.T) {
	want := errors.New("boom")
	var p Prober = ProberFunc(func(context.Context) (any, error) { return nil, want })

	if _, err := p.Probe(context.Background()); !errors.Is(err, want) {
		t.Errorf("Probe error = %v, want %v", err, want)
	}

	p = ProberFunc(func(context.Context) (any, error) { return map[string]int{"n": 1}, nil })
	got, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if m, ok := got.(map[string]int); !ok || m["n"] != 1 {
		t.Errorf("Probe returned %#v", got)
	}
}
