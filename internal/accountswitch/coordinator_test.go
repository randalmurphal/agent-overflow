package accountswitch

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/provider"
)

func TestBeforeSendForcedSwitch(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(96, at(time.Hour)))
	f.setReading("claude", "b", testNow, session(10, at(time.Hour)))
	c := New(f.deps())

	c.BeforeSend("claude", "t1", opusModel, false)

	switches, reports, notices, _ := f.snapshot()
	if !equal(switches, []string{"b"}) {
		t.Fatalf("switches = %v", switches)
	}
	want := Report{Provider: "claude", FromAccountID: "a", ToAccountID: "b", FromLabel: "a@x.com", ToLabel: "b@x.com",
		Reason: ReasonThreshold, LimitName: "Current session", UsedPercent: 96, ResetsAt: at(time.Hour), Model: "claude-opus-4-8"}
	if len(reports) != 1 || reports[0] != want {
		t.Fatalf("reports = %+v", reports)
	}
	wantNotice := "Switched Claude account from a@x.com to b@x.com: a@x.com is at 96% of its Current session limit."
	if len(notices) != 1 || notices[0] != (notice{"t1", wantNotice}) {
		t.Fatalf("notices = %+v", notices)
	}
}

func TestBeforeSendOwnTurnInFlightDoesNotDecide(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	c := New(f.deps())
	c.BeforeSend("claude", "t1", opusModel, true)
	if s, _, _, _ := f.snapshot(); len(s) != 0 {
		t.Fatalf("switches = %v", s)
	}
}

func TestBeforeSendOrderSwitch(t *testing.T) {
	cases := []struct {
		name     string
		pinned   bool
		inflight []ThreadModel
		want     []string
	}{
		{name: "switches to the earlier account", want: []string{"a"}},
		{name: "pinned selection stays", pinned: true},
		{name: "another turn in flight stays", inflight: []ThreadModel{{"t2", opusModel}}},
		{name: "own thread listed in flight does not block", inflight: []ThreadModel{{"t1", opusModel}}, want: []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApp("claude", "b", "a", "b")
			f.pools["claude"].Pinned = tc.pinned
			f.inflight["claude"] = tc.inflight
			c := New(f.deps())
			c.BeforeSend("claude", "t1", opusModel, false)
			switches, _, notices, _ := f.snapshot()
			if !equal(switches, tc.want) {
				t.Fatalf("switches = %v, want %v", switches, tc.want)
			}
			if len(tc.want) > 0 && notices[0].text != "Switched Claude account to a@x.com, the first available in the auto-switch order." {
				t.Fatalf("notice = %q", notices[0].text)
			}
		})
	}
}

func TestPinnedBlocksOrderSwitchNotForced(t *testing.T) {
	f := newFakeApp("claude", "b", "a", "b")
	f.pools["claude"].Pinned = true
	f.setReading("claude", "b", testNow, session(100, at(time.Hour)))
	c := New(f.deps())
	c.BeforeSend("claude", "t1", opusModel, false)
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"a"}) {
		t.Fatalf("switches = %v", s)
	}
}

func TestSelectedOutsidePoolDoesNothing(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.pools["claude"].Accounts[0].InPool = false
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	c := New(f.deps())
	c.BeforeSend("claude", "t1", opusModel, false)
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	if s, _, _, _ := f.snapshot(); len(s) != 0 {
		t.Fatalf("switches = %v", s)
	}
}

func TestNeedsLoginTargetSkipped(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b", "c")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	f.needsLogin["b"] = true
	c := New(f.deps())

	c.BeforeSend("claude", "t1", opusModel, false)
	switches, reports, _, _ := f.snapshot()
	if !equal(switches, []string{"c"}) || len(reports) != 1 || reports[0].Error != "" {
		t.Fatalf("switches = %v reports = %+v", switches, reports)
	}

	// b stays skipped until the account listing changes.
	f.mu.Lock()
	f.pools["claude"].SelectedID = "a"
	f.needsLogin["b"] = false
	f.readings[accountKey{"claude", "c"}] = provider.RateLimitsSnapshot{Limits: []provider.RateLimitEntry{session(100, at(time.Hour))}, UpdatedAt: testNow.UnixMilli()}
	f.mu.Unlock()
	c.BeforeSend("claude", "t1", opusModel, false)
	if s, _, _, _ := f.snapshot(); len(s) != 1 {
		t.Fatalf("switched to a needs-login account: %v", s)
	}
	c.AccountsChanged("claude")
	c.BeforeSend("claude", "t1", opusModel, false)
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"c", "b"}) {
		t.Fatalf("switches after AccountsChanged = %v", s)
	}
}

func TestSwitchFailureReportedOnce(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	f.switchErr["b"] = errors.New("write credentials: disk full")
	c := New(f.deps())

	c.BeforeSend("claude", "t1", opusModel, false)
	c.BeforeSend("claude", "t1", opusModel, false)
	_, reports, notices, _ := f.snapshot()
	if len(reports) != 1 || reports[0].Error != "write credentials: disk full" || reports[0].ToAccountID != "b" {
		t.Fatalf("reports = %+v", reports)
	}
	if len(notices) != 0 {
		t.Fatalf("notices = %+v", notices)
	}

	f.mu.Lock()
	f.switchErr["b"] = errors.New("write credentials: permission denied")
	f.mu.Unlock()
	c.BeforeSend("claude", "t1", opusModel, false)
	if _, reports, _, _ := f.snapshot(); len(reports) != 2 {
		t.Fatalf("a different failure was not reported: %+v", reports)
	}
}

func TestTurnRefusedMarkSeenByBeforeSend(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, fable(20, at(48*time.Hour)))
	c := New(f.deps())

	c.TurnRefused("claude", "t1", fableModel, "a")
	// No background work has run.
	c.BeforeSend("claude", "t1", fableModel, false)
	switches, reports, notices, _ := f.snapshot()
	if !equal(switches, []string{"b"}) || reports[0].Reason != ReasonRefused {
		t.Fatalf("switches = %v reports = %+v", switches, reports)
	}
	if notices[0].text != "Switched Claude account from a@x.com to b@x.com: a@x.com reached a usage limit for claude-fable-5-2." {
		t.Fatalf("notice = %q", notices[0].text)
	}
}

func TestStaleTargetRefreshedOffPath(t *testing.T) {
	cases := []struct {
		name       string
		fresh      bool
		refreshErr error
		want       []string
	}{
		{name: "fresh reading shows exhaustion", fresh: true},
		{name: "failed refresh does not block", refreshErr: errors.New("throttled"), want: []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApp("claude", "a", "a", "b")
			f.inflight["claude"] = []ThreadModel{{"t1", opusModel}, {"t2", fableModel}}
			f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
			f.setReading("claude", "b", testNow.Add(-10*time.Minute), session(10, at(time.Hour)))
			if tc.fresh {
				f.fresh[accountKey{"claude", "b"}] = provider.RateLimitsSnapshot{Limits: []provider.RateLimitEntry{session(100, at(time.Hour))}, UpdatedAt: testNow.UnixMilli()}
			}
			f.refreshErr = tc.refreshErr
			c := New(f.deps())
			c.ReadingUpdated("claude", "a")
			settle(c)
			switches, _, notices, _ := f.snapshot()
			if !equal(f.refreshes, []string{"b"}) || !equal(switches, tc.want) {
				t.Fatalf("refreshes = %v switches = %v", f.refreshes, switches)
			}
			if len(tc.want) > 0 && (len(notices) != 2 || notices[0].threadID != "t1" || notices[1].threadID != "t2") {
				t.Fatalf("reading-caused switch notices = %+v", notices)
			}
		})
	}
}

func TestCodexReadingDoesNotSwitch(t *testing.T) {
	f := newFakeApp("codex", "a", "a", "b")
	f.inflight["codex"] = []ThreadModel{{"t1", "gpt-5.5"}}
	f.setReading("codex", "a", testNow, provider.RateLimitEntry{LimitID: "codex", UsedPercent: 100, ResetsAt: at(time.Hour)})
	c := New(f.deps())
	c.ReadingUpdated("codex", "a")
	settle(c)
	if s, _, _, _ := f.snapshot(); len(s) != 0 {
		t.Fatalf("switches = %v", s)
	}
	// The next send decides.
	c.BeforeSend("codex", "t2", "gpt-5.5", false)
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"b"}) {
		t.Fatalf("switches = %v", s)
	}
}

func TestReadingOfUnselectedAccountDoesNotDecide(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.inflight["claude"] = []ThreadModel{{"t1", opusModel}}
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	c := New(f.deps())
	c.ReadingUpdated("claude", "b")
	settle(c)
	if s, _, _, _ := f.snapshot(); len(s) != 0 {
		t.Fatalf("switches = %v", s)
	}
}

// A trigger that arrives while a switch is in progress decides on the new
// selection. Without serialization it reaches InFlight during the first
// Switch and switches a second time.
func TestTriggerDuringSwitchSeesNewSelection(t *testing.T) {
	triggers := map[string]func(c *Coordinator){
		"send":    func(c *Coordinator) { c.BeforeSend("claude", "t2", opusModel, false) },
		"reading": func(c *Coordinator) { c.ReadingUpdated("claude", "a"); settle(c) },
	}
	for firstName, firstTrigger := range triggers {
		for secondName, second := range triggers {
			t.Run(firstName+" then "+secondName, func(t *testing.T) {
				f := newFakeApp("claude", "a", "a", "b", "c")
				f.inflight["claude"] = []ThreadModel{{"t0", opusModel}}
				f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
				f.setReading("claude", "b", testNow, session(10, at(time.Hour)))
				f.setReading("claude", "c", testNow, session(10, at(time.Hour)))
				c := New(f.deps())
				reached := make(chan struct{}, 8)
				inflight := c.deps.InFlight
				c.deps.InFlight = func(p string) []ThreadModel {
					reached <- struct{}{}
					return inflight(p)
				}
				done := make(chan struct{})
				first := true
				f.beforeSwitch = func(string) {
					if !first {
						return
					}
					first = false
					<-reached // the first trigger's own read
					go func() {
						defer close(done)
						second(c)
					}()
					// Serialized, the second trigger cannot reach InFlight until
					// this switch returns; the short bound ends the wait.
					select {
					case <-reached:
					case <-time.After(10 * time.Millisecond):
					}
				}
				firstTrigger(c)
				<-done
				if s, _, _, _ := f.snapshot(); len(s) != 1 {
					t.Fatalf("switches = %v, want one", s)
				}
			})
		}
	}
}

func TestStopJoinsWithWaitingThreads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeApp("claude", "a", "a")
		f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
		c := New(f.deps(), WithCheckInterval(time.Minute))
		c.Start(context.Background())
		c.TurnRefused("claude", "t1", opusModel, "a")
		synctest.Wait()

		// The check runs on its own while the thread waits.
		f.advance(time.Hour + 10*time.Minute)
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) {
			t.Fatalf("continued = %v", cont)
		}

		c.TurnRefused("claude", "t2", opusModel, "a")
		synctest.Wait()
		if c.waitingCount() != 1 {
			t.Fatal("t2 is not waiting")
		}
		c.Stop()
		c.Stop()
	})
}

// An account left because of a refusal mark comes back once the mark
// expires, although its reading never changed while it was selected.
func TestReturnsToAccountWithUnchangedReading(t *testing.T) {
	cases := map[string]func(c *Coordinator){
		"send":     func(c *Coordinator) { c.BeforeSend("claude", "t2", opusModel, false) },
		"off path": func(c *Coordinator) { settle(c) },
	}
	for name, decideAgain := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeApp("claude", "b", "a", "b")
			f.refreshErr = errors.New("throttled")
			f.setReading("claude", "a", testNow, session(10, at(40*time.Minute)))
			f.setReading("claude", "b", testNow, session(100, at(20*time.Minute)))
			c := New(f.deps())

			c.BeforeSend("claude", "t1", opusModel, false) // b exhausted: to a
			f.advance(21 * time.Minute)
			c.TurnRefused("claude", "t1", opusModel, "a")
			c.BeforeSend("claude", "t1", opusModel, false) // a marked, b reset: to b
			f.advance(20 * time.Minute)
			c.TurnRefused("claude", "t2", opusModel, "b")
			decideAgain(c)

			switches, _, _, _ := f.snapshot()
			if !equal(switches, []string{"a", "b", "a"}) {
				t.Fatalf("switches = %v, want [a b a]", switches)
			}
		})
	}
}

// ReadingUpdated runs on the usage cache's merge path and must not call
// back into the app.
func TestReadingUpdatedCallsNoDependency(t *testing.T) {
	c := New(Deps{})
	c.ReadingUpdated("claude", "a")
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pendingReadings[accountKey{"claude", "a"}] || !c.pendingDeliver["claude"] {
		t.Fatal("reading was not queued")
	}
}

// Without an active pool a send reads only the listing.
func TestBeforeSendWithoutPoolReadsOnlyPool(t *testing.T) {
	cases := map[string]Pool{
		"no accounts":           {SelectedID: "a"},
		"selected outside pool": {SelectedID: "a", Accounts: []Account{{ID: "a", Usable: true}, {ID: "b", InPool: true, Usable: true}}},
		"no other pool account": {SelectedID: "a", Accounts: []Account{{ID: "a", InPool: true, Usable: true}, {ID: "b", Usable: true}}},
		"other needs a sign-in": {SelectedID: "a", Accounts: []Account{{ID: "a", InPool: true, Usable: true}, {ID: "b", InPool: true}}},
	}
	for name, pool := range cases {
		t.Run(name, func(t *testing.T) {
			// Every other dependency is nil, so calling one panics.
			c := New(Deps{Pool: func(string) Pool { return pool }})
			c.BeforeSend("claude", "t1", opusModel, false)
		})
	}
}

// movedUnderTurn runs t1's turn on a while a reading moves the selection to
// b, with the app's switch reporting the move.
func movedUnderTurn(t *testing.T, prov string) (*fakeApp, *Coordinator) {
	t.Helper()
	f := newFakeApp(prov, "a", "a", "b", "c")
	f.reaches[prov] = prov == "claude"
	// The app may list the sending thread while its send starts.
	f.inflight[prov] = []ThreadModel{{"t1", opusModel}, {"t2", opusModel}}
	wide := func(pct float64) provider.RateLimitEntry {
		e := session(pct, at(time.Hour))
		if prov == "codex" {
			e.LimitID = "codex"
		}
		return e
	}
	f.setReading(prov, "a", testNow, wide(100))
	f.setReading(prov, "b", testNow, wide(5))
	f.setReading(prov, "c", testNow, wide(5))
	c := New(f.deps())
	f.switched = func(p, from, to string) { c.SelectionMoved(p, from, to) }
	c.BeforeSend(prov, "t2", opusModel, false)
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"b"}) {
		t.Fatalf("switches = %v, want [b]", s)
	}
	return f, c
}

func (c *Coordinator) marked(prov, accountID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.marks[accountKey{prov, accountID}]) > 0
}

// A turn that was running when the selection moved can be refused on either
// account, and more than once (a subagent, then its parent). None of those
// refusals marks an account: both accounts are read again, once, and with
// the selected account still clear the thread continues on it. The turn the
// next send starts runs on one account, so its refusal marks that account.
func TestRefusalsOfMovedTurnMarkNoAccount(t *testing.T) {
	f, c := movedUnderTurn(t, "claude")
	c.mu.Lock()
	_, sendingRecorded := c.movedTurns["t2"]
	c.mu.Unlock()
	if sendingRecorded {
		t.Fatal("the sending thread was recorded as a moved turn")
	}
	later := testNow.Add(time.Second).UnixMilli()
	f.fresh[accountKey{"claude", "a"}] = provider.RateLimitsSnapshot{Limits: []provider.RateLimitEntry{session(100, at(time.Hour))}, UpdatedAt: later}
	f.fresh[accountKey{"claude", "b"}] = provider.RateLimitsSnapshot{Limits: []provider.RateLimitEntry{session(5, at(time.Hour))}, UpdatedAt: later}

	c.TurnRefused("claude", "t1", opusModel, "b")
	c.TurnRefused("claude", "t1", opusModel, "b")
	settle(c)
	if c.marked("claude", "a") || c.marked("claude", "b") {
		t.Fatalf("marks a=%v b=%v, want none", c.marked("claude", "a"), c.marked("claude", "b"))
	}
	switches, _, _, cont := f.snapshot()
	if !equal(switches, []string{"b"}) || !equal(f.refreshes, []string{"a", "b"}) || !equal(cont, []string{"t1"}) {
		t.Fatalf("switches = %v refreshes = %v continued = %v, want b kept, a and b read once, t1 continued once", switches, f.refreshes, cont)
	}

	c.BeforeSend("claude", "t1", opusModel, false)
	c.TurnRefused("claude", "t1", opusModel, "b")
	if c.marked("claude", "a") || !c.marked("claude", "b") {
		t.Fatal("a refusal of the turn the send started was not charged to b alone")
	}
}

// The readings taken after such a refusal decide: when the account switched
// to turns out to be exhausted, the selection moves on and the thread
// continues there, and the account the turn started on carries no mark.
func TestRefusalAfterSwitchFollowsFreshReadings(t *testing.T) {
	f, c := movedUnderTurn(t, "claude")
	f.fresh[accountKey{"claude", "b"}] = provider.RateLimitsSnapshot{
		Limits: []provider.RateLimitEntry{session(100, at(time.Hour))}, UpdatedAt: testNow.UnixMilli(),
	}
	c.TurnRefused("claude", "t1", opusModel, "b")
	settle(c)
	switches, _, _, cont := f.snapshot()
	if !equal(switches, []string{"b", "c"}) || !equal(cont, []string{"t1"}) {
		t.Fatalf("switches = %v continued = %v, want a move on to c and t1 continued", switches, cont)
	}
	if c.marked("claude", "a") || c.marked("claude", "b") {
		t.Fatal("a refusal that could belong to either account marked one")
	}

	// The same turn has now used a and b before c.
	c.TurnRefused("claude", "t1", opusModel, "c")
	settle(c)
	if c.marked("claude", "a") || c.marked("claude", "b") || c.marked("claude", "c") {
		t.Fatal("a refusal after a second move marked an account")
	}
	if !equal(f.refreshes[len(f.refreshes)-3:], []string{"a", "b", "c"}) {
		t.Fatalf("refreshes = %v, want a, b and c read again", f.refreshes)
	}
}

func TestMovedTurnExpires(t *testing.T) {
	f, c := movedUnderTurn(t, "claude")
	f.advance(originWindow + time.Second)
	c.TurnRefused("claude", "t1", opusModel, "b")
	if c.marked("claude", "a") || !c.marked("claude", "b") {
		t.Fatal("a refusal long after the move was not charged to the reported account")
	}
}

func TestMovedTurnEndsAtNextSend(t *testing.T) {
	_, first := movedUnderTurn(t, "claude")
	first.BeforeSend("claude", "t1", opusModel, true)
	first.TurnRefused("claude", "t1", opusModel, "b")
	if first.marked("claude", "b") {
		t.Fatal("a send into the running turn ended its moved-turn record")
	}
	f, c := movedUnderTurn(t, "claude")
	// Also without an active pool, where the send makes no decision.
	f.mu.Lock()
	for i := range f.pools["claude"].Accounts {
		f.pools["claude"].Accounts[i].InPool = false
	}
	f.mu.Unlock()
	c.BeforeSend("claude", "t1", opusModel, false)
	c.TurnRefused("claude", "t1", opusModel, "b")
	if c.marked("claude", "a") || !c.marked("claude", "b") {
		t.Fatal("a new turn's refusal was charged to the previous turn's account")
	}
}

func TestCodexSelectionMoveRecordsNothing(t *testing.T) {
	_, c := movedUnderTurn(t, "codex")
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.movedTurns) != 0 {
		t.Fatalf("moved turns = %v", c.movedTurns)
	}
}

// The listing change a switch publishes keeps the needs-login memory; one
// from outside a switch clears it.
func TestNeedsLoginSurvivesOwnSwitch(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b", "c", "d")
	f.setReading("claude", "a", testNow, session(100, at(time.Hour)))
	f.setReading("claude", "c", testNow, session(5, at(time.Hour)))
	f.setReading("claude", "d", testNow, session(5, at(time.Hour)))
	f.needsLogin["b"] = true
	c := New(f.deps())
	f.switched = func(p, _, _ string) { c.AccountsChanged(p) }

	c.BeforeSend("claude", "t1", opusModel, false)
	f.setReading("claude", "c", testNow, session(100, at(time.Hour)))
	c.BeforeSend("claude", "t1", opusModel, false)
	if s, _, _, _ := f.snapshot(); !equal(s, []string{"c", "d"}) || f.needsLoginAttempts["b"] != 1 {
		t.Fatalf("switches = %v, b attempts = %d", s, f.needsLoginAttempts["b"])
	}

	c.AccountsChanged("claude")
	f.setReading("claude", "d", testNow, session(100, at(time.Hour)))
	c.BeforeSend("claude", "t1", opusModel, false)
	if f.needsLoginAttempts["b"] != 2 {
		t.Fatalf("b attempts = %d after an outside listing change, want 2", f.needsLoginAttempts["b"])
	}
}
