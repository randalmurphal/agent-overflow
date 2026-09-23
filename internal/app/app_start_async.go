package app

import "context"

// asyncStart is a Start running on its own goroutine: the desktop boot,
// whose window opens while the App starts behind it.
type asyncStart struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startAsync runs Start on its own goroutine and hands its result to the
// startDone hook. A Start that ends because stopAsyncStart canceled it
// reports nothing: the process is already shutting down, and that is not
// a startup failure. The context Start receives lives until
// stopAsyncStart, as the headless boot's does until process exit.
func (a *App) startAsync(ctx context.Context) {
	startCtx, cancel := context.WithCancel(ctx)
	run := &asyncStart{cancel: cancel, done: make(chan struct{})}
	a.asyncStart.Store(run)
	go func() {
		defer close(run.done)
		err := a.Start(startCtx)
		if startCtx.Err() != nil {
			return
		}
		if a.startDone != nil {
			a.startDone(err)
		}
	}()
}

// stopAsyncStart cancels a Start begun by startAsync and waits for it to
// return, so Shutdown never runs beside it. Start checks the cancellation
// between phases and inside migrations; a phase that does not observe it
// finishes first.
func (a *App) stopAsyncStart() {
	run := a.asyncStart.Load()
	if run == nil {
		return
	}
	run.cancel()
	<-run.done
}
