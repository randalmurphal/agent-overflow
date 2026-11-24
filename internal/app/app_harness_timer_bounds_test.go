package app

import (
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/pump"
)

// At the longest value a harness boot may give each timer the App doubles
// or multiplies (main_harness.go harnessTimers), that arithmetic stays a
// positive Duration no longer than its cap; one step longer is refused at
// parse (TestParseHarnessTimingRefusesWhatTheDestinationCannotHold).
func TestHarnessTimerBoundsKeepTheirArithmeticInRange(t *testing.T) {
	t.Parallel()
	app := NewApp()

	app.wakeRetryBaseOverride = MaxThreadWakeRetryBase
	ceiling := MaxThreadWakeRetryBase * threadWakeRetryCeiling
	for attempts := int64(1); attempts <= threadWakeRetryAttempts+4; attempts++ {
		if delay := app.threadWakeRetryDelay(attempts); delay < MaxThreadWakeRetryBase || delay > ceiling {
			t.Fatalf("wake retry %d at the longest base = %v, want %v to %v", attempts, delay, MaxThreadWakeRetryBase, ceiling)
		}
	}
	if delay := app.threadWakeRetryDelay(threadWakeRetryAttempts + 4); delay != ceiling {
		t.Fatalf("a late wake retry at the longest base = %v, want the ceiling %v", delay, ceiling)
	}

	now := time.Now()
	var backoff mergedPRBackoff
	for range mergedPRPollBackoffDoublings + 2 {
		backoff.fail(now, MaxMergedPRPollInterval, errors.New("forge down"))
		if !backoff.next.After(now) {
			t.Fatalf("merged-PR backoff after %d failures at the longest poll = %v, before %v", backoff.failures, backoff.next, now)
		}
	}

	for attempts := range int64(4) {
		if delay := threadLatePollDelay(MaxThreadRequestPoll, attempts+1); delay != threadPollLateCap {
			t.Fatalf("late poll %d at the longest poll = %v, want the cap %v", attempts+1, delay, threadPollLateCap)
		}
	}

	app.prUpdates.interval = MaxPRUpdateInterval
	app.prUpdates.ciLiveInterval = MaxPRCILiveInterval
	streams := false
	app.prUpdates.ciLogStreams = &streams
	phase := &ciPhase{repo: testPR.Repository(), ciFail: prForgeFailure{key: "down"}}
	for range 4 {
		if delay := app.ciInterval(phase); delay < MaxPRCILiveInterval || delay > MaxPRUpdateInterval {
			t.Fatalf("CI retry at the longest live and update intervals = %v, want %v to %v", delay, MaxPRCILiveInterval, MaxPRUpdateInterval)
		}
	}

	// Enough doublings from the product base to reach the cap and pass it.
	var delay time.Duration
	for range 64 {
		if delay = pump.NextDelay(delay, defaultPRUpdateRetryBase, MaxPRUpdateInterval); delay <= 0 {
			t.Fatalf("pump retry under the longest PR update interval = %v", delay)
		}
	}
}
