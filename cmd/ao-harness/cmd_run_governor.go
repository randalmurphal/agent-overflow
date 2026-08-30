package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harnessrun"
)

type runGovernor struct {
	mgr         *governor.Manager
	lease       governor.Lease
	cancel      context.CancelFunc
	done        chan error
	events      chan governor.Event
	cleanupOnce sync.Once
	cleanupErr  error
}

func (g *runGovernor) cleanup(ctx context.Context) error {
	if g == nil || g.mgr == nil {
		return nil
	}
	g.cleanupOnce.Do(func() {
		if g.cancel != nil {
			g.cancel()
		}
		if g.done != nil {
			select {
			case monitorErr := <-g.done:
				g.cleanupErr = monitorErr
			case <-ctx.Done():
				g.cleanupErr = ctx.Err()
				go func() {
					<-g.done
					_ = g.mgr.Release(g.lease)
				}()
			}
		}
		if g.cleanupErr == nil {
			g.cleanupErr = g.mgr.Release(g.lease)
		}
	})
	return g.cleanupErr
}

func (g *runGovernor) safetyError() error {
	if g == nil || g.events == nil {
		return nil
	}
	select {
	case event := <-g.events:
		if event.Reason == governor.ReasonAvailableFloor {
			return fmt.Errorf("host available memory fell below floor: available=%d floor=%d (%s)", event.AvailableBytes, event.AvailableFloorBytes, event.Reason)
		}
		if event.Reason == governor.ReasonMonitorError {
			return fmt.Errorf("run memory monitor failed: %s", event.Error)
		}
		return fmt.Errorf("run exceeded memory ceiling: rss=%d ceiling=%d (%s)", event.RSSBytes, event.CeilingBytes, event.Reason)
	default:
		return nil
	}
}

func startRunGovernor(ctx context.Context, plan harnessrun.RunPlan, ownerPID int, onSafety func()) (*runGovernor, error) {
	if plan.Ceiling.MaxPrivateBytes == 0 {
		return &runGovernor{}, nil
	}
	worktree, _ := os.Getwd()
	mgr, err := governor.New(governor.Options{})
	if err != nil {
		return nil, err
	}
	if ownerPID <= 0 {
		ownerPID = os.Getpid()
	}
	lease, err := mgr.Reserve(governor.Request{RunID: plan.RunID, Worktree: worktree, DataRoot: plan.DataRoot, OwnerPID: ownerPID, CeilingBytes: plan.Ceiling.MaxPrivateBytes})
	if err != nil {
		return nil, err
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	events := make(chan governor.Event, 1)
	go func() {
		monitorErr := mgr.Monitor(monitorCtx, lease, 500*time.Millisecond, nil, func(event governor.Event) {
			select {
			case events <- event:
			default:
			}
			if onSafety != nil {
				onSafety()
			}
		})
		if monitorErr != nil {
			event := governor.Event{RunID: lease.RunID, Worktree: lease.Worktree, DataRoot: lease.DataRoot, Reason: governor.ReasonMonitorError, Error: monitorErr.Error(), At: time.Now().UTC()}
			select {
			case events <- event:
			default:
			}
			if onSafety != nil {
				onSafety()
			}
		}
		done <- monitorErr
	}()
	return &runGovernor{mgr: mgr, lease: lease, cancel: cancel, done: done, events: events}, nil
}
