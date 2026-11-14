package accountswitch

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestBeforeSendClearsWaiting(t *testing.T) {
	f := newFakeApp("claude", "a", "a")
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	if c.waitingCount() != 1 {
		t.Fatal("refused thread is not waiting")
	}
	c.BeforeSend("claude", "t1", opusModel, true)
	if c.waitingCount() != 0 {
		t.Fatal("send did not clear waiting")
	}
	settle(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued = %v", cont)
	}
}

func TestContinueAfterSwitchOnce(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	f.setReading("claude", "b", testNow, session(10, at(time.Hour)))
	c := New(f.deps())

	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	c.ReadingUpdated("claude", "b")
	check(c)
	settle(c)

	switches, _, notices, cont := f.snapshot()
	if !equal(switches, []string{"b"}) || !equal(cont, []string{"t1"}) {
		t.Fatalf("switches = %v continued = %v", switches, cont)
	}
	if len(notices) != 1 || notices[0].threadID != "t1" {
		t.Fatalf("notices = %+v", notices)
	}
	if c.waitingCount() != 0 {
		t.Fatal("thread still waiting after continue")
	}
}

func TestContinueNotSentToRejectedThread(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	f.rejects["workflow"] = true
	c := New(f.deps())

	c.TurnRefused("claude", "workflow", opusModel, "a")
	if c.waitingCount() != 0 {
		t.Fatal("rejected thread is waiting")
	}
	// A thread archived after its refusal is dropped at delivery.
	c.TurnRefused("claude", "t1", opusModel, "a")
	f.mu.Lock()
	f.rejects["t1"] = true
	f.mu.Unlock()
	settle(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued = %v", cont)
	}
	if c.waitingCount() != 0 {
		t.Fatal("archived thread still waiting")
	}
}

func TestContinueDroppedWhenSettingOff(t *testing.T) {
	f := newFakeApp("claude", "a", "a")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)

	// Off, before the coordinator is told: nothing is sent.
	off := f.write(func(cfg *Config) { cfg.AutoContinue = false })
	f.advance(2 * time.Hour)
	check(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued with the setting off: %v", cont)
	}
	c.RevokeWaitsBefore(off)
	if c.waitingCount() != 0 {
		t.Fatal("waiting kept after the setting turned off")
	}
	c.RevokeWaitsBefore(f.write(func(cfg *Config) { cfg.AutoContinue = true }))
	check(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued = %v", cont)
	}
}

// A wait admitted before auto-continue was switched off does not survive a
// quick off and on, in whichever order the two writes are observed. A wait
// admitted after the last write stays, and a refusal that read the settings
// before a write does not start one.
func TestWaitRevokedByEarlierWriteToTheSetting(t *testing.T) {
	for name, order := range map[string][]int{"off observed first": {0, 1}, "on observed first": {1, 0}} {
		t.Run(name, func(t *testing.T) {
			f := newFakeApp("claude", "a", "a")
			f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
			c := New(f.deps())
			c.TurnRefused("claude", "before", opusModel, "a")
			stale := f.deps().Settings()

			// Both writes land before either is observed.
			writes := []uint64{
				f.write(func(cfg *Config) { cfg.AutoContinue = false }),
				f.write(func(cfg *Config) { cfg.AutoContinue = true }),
			}
			c.TurnRefused("claude", "after", opusModel, "a")
			for _, i := range order {
				c.RevokeWaitsBefore(writes[i])
			}
			if c.isWaiting("before") || !c.isWaiting("after") {
				t.Fatalf("waiting before=%v after=%v, want only the wait admitted after the writes", c.isWaiting("before"), c.isWaiting("after"))
			}

			f.mu.Lock()
			f.cfg = stale
			f.mu.Unlock()
			c.TurnRefused("claude", "stale", opusModel, "a")
			if c.isWaiting("stale") {
				t.Fatal("a refusal that read the settings before the writes is waiting")
			}
		})
	}
}

func TestContinueAfterConfirmedReset(t *testing.T) {
	f := newFakeApp("claude", "a", "a")
	reset := testNow.Add(10 * time.Minute)
	f.setReading("claude", "a", testNow, session(100, reset.Unix()))
	c := New(f.deps())

	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	_, _, notices, _ := f.snapshot()
	want := "This account is out of usage for this model until Fri 12:10 PM. This thread continues then."
	if len(notices) != 1 || notices[0].text != want {
		t.Fatalf("notices = %+v", notices)
	}

	// Past the reset with only an older reading: not yet, and a reading is
	// requested.
	f.advance(11 * time.Minute)
	f.mu.Lock()
	before := f.requests
	f.mu.Unlock()
	check(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued before confirmation: %v", cont)
	}
	if f.requests == before {
		t.Fatal("no reading requested for an unconfirmed reset")
	}

	f.setReading("claude", "a", reset.Add(time.Second), session(0, reset.Add(5*time.Hour).Unix()))
	c.ReadingUpdated("claude", "a")
	settle(c)
	settle(c)
	if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) {
		t.Fatalf("continued = %v", cont)
	}
}

func TestContinueFiveMinutesAfterReset(t *testing.T) {
	f := newFakeApp("claude", "a", "a")
	reset := testNow.Add(10 * time.Minute)
	f.setReading("claude", "a", testNow, session(100, reset.Unix()))
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)

	f.advance(14*time.Minute + 59*time.Second)
	check(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 0 {
		t.Fatalf("continued before the fallback: %v", cont)
	}
	f.advance(time.Second)
	check(c)
	check(c)
	if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) {
		t.Fatalf("continued = %v", cont)
	}
}

func TestMinuteCheckSwitchesToResetAccount(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(2*time.Hour)))
	f.setReading("claude", "b", testNow, session(100, at(30*time.Minute)))
	c := New(f.deps())

	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	switches, _, notices, _ := f.snapshot()
	want := "No account in the auto-switch pool can run this model right now. The earliest reset is b@x.com at Fri 12:30 PM. This thread continues then."
	if len(switches) != 0 || len(notices) != 1 || notices[0].text != want {
		t.Fatalf("switches = %v notices = %+v", switches, notices)
	}

	// b reset; its reading predates the reset and is stale, so it is read
	// again before the switch.
	f.advance(31 * time.Minute)
	f.fresh[accountKey{"claude", "b"}] = provider.RateLimitsSnapshot{Limits: []provider.RateLimitEntry{session(0, at(6*time.Hour))}, UpdatedAt: f.now.UnixMilli()}
	check(c)
	settle(c)
	switches, _, _, cont := f.snapshot()
	if !equal(switches, []string{"b"}) || !equal(f.refreshes, []string{"b"}) || !equal(cont, []string{"t1"}) {
		t.Fatalf("switches = %v refreshes = %v continued = %v", switches, f.refreshes, cont)
	}
}

func TestContinueErrorReachesThread(t *testing.T) {
	f := newFakeApp("claude", "a", "a")
	f.continueErr = errors.New("thread busy")
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	f.advance(2 * time.Hour)
	settle(c)
	_, _, notices, _ := f.snapshot()
	if len(notices) == 0 || notices[len(notices)-1].text != "Could not continue this thread after the usage limit: thread busy" {
		t.Fatalf("notices = %+v", notices)
	}
	if c.waitingCount() != 0 {
		t.Fatal("failed continue left the thread waiting")
	}
}

// guessedContinue refuses t1 on an account with no reading, so the mark's
// reset is the one-hour guess, and delivers the continue.
func guessedContinue(t *testing.T) (*fakeApp, *Coordinator) {
	t.Helper()
	f := newFakeApp("claude", "a", "a")
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	f.advance(time.Hour + 6*time.Minute)
	check(c)
	if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) {
		t.Fatalf("continued = %v", cont)
	}
	return f, c
}

// A guessed refusal soon after a continue on a guessed wait stops the
// waiting, however many times the send path ran in between.
func TestGuessedRetryStops(t *testing.T) {
	f, c := guessedContinue(t)
	for range 3 {
		c.BeforeSend("claude", "t1", opusModel, false)
	}
	f.advance(guessedRetryWindow)
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	if c.waitingCount() != 0 {
		t.Fatal("thread waits again after a second guessed refusal")
	}
	_, _, notices, _ := f.snapshot()
	if got := notices[len(notices)-1].text; got != giveUpNotice {
		t.Fatalf("last notice = %q", got)
	}
	f.advance(3 * time.Hour)
	check(c)
	if _, _, _, cont := f.snapshot(); len(cont) != 1 {
		t.Fatalf("continued = %v", cont)
	}
}

func TestGuessedRetryWaitsAgainAfterWindow(t *testing.T) {
	f, c := guessedContinue(t)
	f.advance(guessedRetryWindow + time.Second)
	c.TurnRefused("claude", "t1", opusModel, "a")
	if c.waitingCount() != 1 {
		t.Fatal("refusal after the window does not wait")
	}
}

func TestGuessedRetryWaitsAgainWhenMarkNamesLimit(t *testing.T) {
	f, c := guessedContinue(t)
	f.setReading("claude", "a", f.now, session(100, f.now.Add(time.Hour).Unix()))
	c.TurnRefused("claude", "t1", opusModel, "a")
	if c.waitingCount() != 1 {
		t.Fatal("refusal with a known reset does not wait")
	}
	// Its continue followed a known reset, so a guessed refusal after it
	// still waits.
	f.advance(time.Hour + 6*time.Minute)
	f.setReading("claude", "a", f.now, session(0, f.now.Add(5*time.Hour).Unix()))
	check(c)
	f.setReading("claude", "a", f.now)
	c.TurnRefused("claude", "t1", opusModel, "a")
	if _, _, _, cont := f.snapshot(); len(cont) != 2 || c.waitingCount() != 1 {
		t.Fatalf("continued = %v waiting = %d", cont, c.waitingCount())
	}
}

// A continue whose thread lock wait timed out stays waiting for the next
// check; one cancelled by shutdown writes nothing.
func TestContinueContextErrors(t *testing.T) {
	t.Run("deadline retries", func(t *testing.T) {
		f, c := readyToContinue()
		f.continueHook = func(context.Context, string) error { return context.DeadlineExceeded }
		check(c)
		if c.waitingCount() != 1 {
			t.Fatal("timed-out continue dropped the wait")
		}
		f.continueHook = nil
		check(c)
		if _, _, notices, cont := f.snapshot(); !equal(cont, []string{"t1"}) || len(notices) != 0 {
			t.Fatalf("continued = %v notices = %+v", cont, notices)
		}
	})
	t.Run("cancel is silent", func(t *testing.T) {
		f, c := readyToContinue()
		ctx, cancel := context.WithCancel(context.Background())
		f.continueHook = func(context.Context, string) error {
			cancel()
			return context.Canceled
		}
		c.checkWaiting(ctx)
		c.deliveries.Wait()
		c.TurnRefused("claude", "t2", opusModel, "a")
		c.runPending(ctx)
		c.handleRefusal(ctx, refusal{provider: "claude", threadID: "t2", model: opusModel})
		if _, _, notices, _ := f.snapshot(); len(notices) != 0 {
			t.Fatalf("notices during shutdown = %+v", notices)
		}
	})
	t.Run("cancel during a refusal is silent", func(t *testing.T) {
		f := newFakeApp("claude", "a", "a")
		f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
		c := New(f.deps())
		ctx, cancel := context.WithCancel(context.Background())
		f.requestHook = cancel
		c.TurnRefused("claude", "t1", opusModel, "a")
		c.runPending(ctx)
		if _, _, notices, _ := f.snapshot(); len(notices) != 0 {
			t.Fatalf("notices after cancellation = %+v", notices)
		}
	})
}

// readyToContinue leaves t1 waiting with its limit reset, so the next check
// delivers it.
func readyToContinue() (*fakeApp, *Coordinator) {
	f := newFakeApp("claude", "a", "a")
	f.cfg.AutoContinue = true
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	c.mu.Lock()
	c.pendingRefusals = nil
	c.mu.Unlock()
	f.advance(2 * time.Hour)
	return f, c
}

// The continue check runs again under the thread lock. When the reading, the
// thread's model or the selection changed while the delivery waited for the
// lock, nothing is queued and the thread keeps waiting for the next check.
func TestContinueRecheckedAtAdmission(t *testing.T) {
	now := testNow.Add(2 * time.Hour)
	reset := now.Add(time.Hour).Unix()
	changes := map[string]func(f *fakeApp){
		"reading": func(f *fakeApp) {
			f.setReading("claude", "a", now, session(100, reset))
		},
		"model": func(f *fakeApp) {
			f.setReading("claude", "a", now, fable(100, reset))
			f.mu.Lock()
			f.models["t1"] = fableModel
			f.mu.Unlock()
		},
		"selection": func(f *fakeApp) {
			f.setReading("claude", "b", now, session(100, reset))
			f.mu.Lock()
			pool := f.pools["claude"]
			pool.Accounts = append(pool.Accounts, Account{ID: "b", Label: "b@x.com", InPool: true, Usable: true})
			pool.SelectedID = "b"
			f.mu.Unlock()
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f, c := readyToContinue()
			f.lockHook = func() { change(f) }
			check(c)
			if _, _, _, cont := f.snapshot(); len(cont) != 0 || c.waitingCount() != 1 {
				t.Fatalf("continued = %v waiting = %d, want nothing queued and the wait kept", cont, c.waitingCount())
			}

			f.mu.Lock()
			f.lockHook = nil
			f.mu.Unlock()
			f.advance(time.Hour + resetGrace)
			check(c)
			if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) || c.waitingCount() != 0 {
				t.Fatalf("continued = %v waiting = %d after the limit reset", cont, c.waitingCount())
			}
		})
	}
}

// A continue that blocks does not hold up decisions of the worker.
func TestSlowContinueDoesNotBlockDecisions(t *testing.T) {
	f, c := readyToContinue()
	f.pools["claude"].Accounts = append(f.pools["claude"].Accounts, Account{ID: "b", Label: "b@x.com", InPool: true, Usable: true})
	release := make(chan struct{})
	entered := make(chan struct{})
	f.continueHook = func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	}
	c.checkWaiting(context.Background())
	<-entered
	f.setReading("claude", "a", f.now, session(100, f.now.Add(time.Hour).Unix()))
	c.BeforeSend("claude", "t2", opusModel, false)
	c.TurnRefused("claude", "t3", opusModel, "b")
	c.runPending(context.Background())
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"b"}) {
		t.Fatalf("switches = %v while a continue blocked", s)
	}
	close(release)
	c.deliveries.Wait()
}

// A waiting thread of a provider with no selected account is dropped, so
// the periodic check stops.
func TestWaiterWithoutSelectionDropped(t *testing.T) {
	f, c := readyToContinue()
	f.pools["claude"].SelectedID = ""
	check(c)
	if c.waitingCount() != 0 {
		t.Fatal("waiter kept without a selected account")
	}
}
