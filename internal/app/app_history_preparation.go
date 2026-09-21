package app

import (
	"context"
	"errors"
	"log"
	"time"
)

// Preparation uses the existing store maintenance lifetime, but keeps its
// own loop because live history can grow while vacuum waits for quiet.
func (a *App) startHistoryPreparation() {
	if a.store == nil {
		return
	}
	stop, started := a.historyPreparation.start()
	if !started {
		return
	}
	go func() {
		defer a.historyPreparation.done()
		ctx, cancel := context.WithCancel(a.lifeCtx())
		watcherDone := make(chan struct{})
		defer func() {
			cancel()
			<-watcherDone
		}()
		go func() {
			defer close(watcherDone)
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		a.runHistoryPreparation(ctx)
	}()
}

func (a *App) runHistoryPreparation(ctx context.Context) {
	after := ""
	progress := false
	pause := func(delay time.Duration) bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	for ctx.Err() == nil {
		threads, err := a.store.HistoryPreparationThreads(ctx, after)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("history preparation: %v", err)
			}
			if !pause(time.Second) {
				return
			}
			continue
		}
		if len(threads) == 0 {
			after = ""
			delay := time.Second
			if progress {
				delay = 25 * time.Millisecond
			}
			progress = false
			if !pause(delay) {
				return
			}
			continue
		}
		for _, thread := range threads {
			n, err := a.store.PrepareThreadHistory(ctx, thread)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("history preparation %s: %v", thread, err)
			}
			progress = progress || n > 0
			after = thread
			if !pause(25 * time.Millisecond) {
				return
			}
		}
	}
}
