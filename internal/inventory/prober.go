// Package inventory collects, schedules and stores the host inventory.
package inventory

import (
	"context"

	"github.com/krisiasty/hwi-api/apiv1"
)

// Prober collects one subsystem. The returned value is marshalled by the
// store; probers do no formatting of their own.
type Prober interface {
	Probe(ctx context.Context) (any, error)
}

// ProberFunc adapts a plain function to Prober.
type ProberFunc func(ctx context.Context) (any, error)

// Probe implements Prober.
func (f ProberFunc) Probe(ctx context.Context) (any, error) { return f(ctx) }

// Probers maps each subsystem to its collector.
type Probers map[apiv1.Subsystem]Prober
