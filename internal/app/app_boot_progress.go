package app

import (
	"context"
	"fmt"
	"time"
)

// BootProgress receives App.Start's phases for the readiness report that a
// not-ready bootstrap serves (transport.StartupReporter). Phase ids are the
// `boot: phase=` log names; details are sentences a person reads.
type BootProgress interface {
	BeginBootPhase(phase, detail string) (end func())
	BootPhaseDetail(detail string, step, steps int)
}

// bootPhase begins a reported boot phase. The returned end reports the
// phase finished and logs its duration under the same id.
func (a *App) bootPhase(phase, detail string) (end func()) {
	started := time.Now()
	endReport := func() {}
	if a.bootProgress != nil {
		endReport = a.bootProgress.BeginBootPhase(phase, detail)
	}
	return func() {
		endReport()
		logBootPhase(phase, started)
	}
}

// bootPhaseDetail reports progress inside the innermost open boot phase.
func (a *App) bootPhaseDetail(detail string, step, steps int) {
	if a.bootProgress != nil {
		a.bootProgress.BootPhaseDetail(detail, step, steps)
	}
}

// bootCanceled reports whether the boot was asked to stop before next
// began. Start returns the error, and the caller tears the partial boot
// down the way it does any failed Start.
func bootCanceled(ctx context.Context, next string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("startup stopped before %s: %w", next, err)
	}
	return nil
}
