package app

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/store"
)

// BootProgress receives App.Start's phases for the readiness report that a
// not-ready bootstrap serves (transport.StartupReporter). Phase ids are the
// `boot: phase=` log names; details are sentences a person reads.
// WatchBootFiles names files whose size changes count as progress while a
// phase runs, so a long statement that writes reads as working.
type BootProgress interface {
	BeginBootPhase(phase, detail string) (end func())
	BootPhaseDetail(detail string, step, steps int)
	WatchBootFiles(paths ...string)
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

// BeginBootPhase reports a boot phase that runs outside App.Start, such as
// the harness's startup hold, on the same readiness report.
func BeginBootPhase(a *App, phase, detail string) (end func()) {
	return a.bootPhase(phase, detail)
}

// bootPhaseDetail reports progress inside the innermost open boot phase.
func (a *App) bootPhaseDetail(detail string, step, steps int) {
	if a.bootProgress != nil {
		a.bootProgress.BootPhaseDetail(detail, step, steps)
	}
}

// reportMigration turns the store's migration steps into the
// store.migrate boot phase, which is what a launcher waiting on a long
// migration chain reads. The first pending migration begins the phase and
// stores its end in end. Each migration is a step, and a rebuild's index
// builds and foreign key check are details of their migration's step.
func (a *App) reportMigration(end *func()) func(store.MigrationStep) {
	return func(step store.MigrationStep) {
		if step.Index == 1 && step.Activity == "" {
			*end = a.bootPhase("store.migrate", "Applying migrations")
		}
		detail := fmt.Sprintf("Applying migration %d of %d %s", step.Index, step.Pending, step.Name)
		if step.Activity != "" {
			detail += ": " + step.Activity
		}
		a.bootPhaseDetail(detail, step.Index, step.Pending)
	}
}

// watchBootFiles reports files whose size changes count as boot progress.
func (a *App) watchBootFiles(paths ...string) {
	if a.bootProgress != nil {
		a.bootProgress.WatchBootFiles(paths...)
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
