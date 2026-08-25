package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

func TestMapSubmitPRReviewResult(t *testing.T) {
	cases := []struct {
		name    string
		result  gitops.SubmitReviewResult
		err     error
		want    SubmitPRReviewResult
		wantErr bool
	}{
		{
			name:   "success",
			result: gitops.SubmitReviewResult{PostedReview: true, PostedFileComments: 2},
			want:   SubmitPRReviewResult{PostedReview: true, PostedFileComments: 2},
		},
		{
			name: "partial",
			err: &gitops.PartialSubmitError{
				PostedReview:       true,
				PostedFileComments: 1,
				FailedPath:         "README.md",
				Err:                errors.New("denied"),
			},
			want: SubmitPRReviewResult{
				PostedReview:       true,
				PostedFileComments: 1,
				PartialFailurePath: "README.md",
				PartialFailure:     "denied",
			},
		},
		{
			name:    "hard error",
			err:     errors.New("no auth"),
			wantErr: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapSubmitPRReviewResult(tt.result, tt.err)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

// testPR is the reference every pump test polls; prUpdateKey(testPR) is the
// wire key its events carry.
var testPR = gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}

// capturePRUpdates routes "pr:updated" emissions into a channel. Buffered
// deep enough that a pump ticking during an assertion cannot block.
func capturePRUpdates(t *testing.T, app *App) chan PRUpdatedEvent {
	t.Helper()
	events := make(chan PRUpdatedEvent, 16)
	app.testEmitHook = func(name string, data any) {
		if name != "pr:updated" {
			return
		}
		evt, ok := data.(PRUpdatedEvent)
		if !ok {
			t.Errorf("event payload type = %T", data)
			return
		}
		events <- evt
	}
	return events
}

func awaitPRUpdate(t *testing.T, events <-chan PRUpdatedEvent, why string) PRUpdatedEvent {
	t.Helper()
	select {
	case evt := <-events:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
		return PRUpdatedEvent{}
	}
}

func expectNoPRUpdate(t *testing.T, events <-chan PRUpdatedEvent, why string) {
	t.Helper()
	select {
	case evt := <-events:
		t.Fatalf("unexpected emit (%s): %+v", why, evt)
	case <-time.After(60 * time.Millisecond):
	}
}

// prPumpState reads a PR's pump bookkeeping under the App's lock.
func prPumpState(app *App, prKey string) (refs, active int, paused, present bool) {
	app.prUpdates.mu.Lock()
	defer app.prUpdates.mu.Unlock()
	pump, ok := app.prUpdates.pumps[prKey]
	if !ok {
		return 0, 0, false, false
	}
	return pump.refs, pump.active, pump.paused.Load(), true
}

func TestPRUpdatePollingEmitsOnlyOnSnapshotChange(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	calls := 0
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls++
		head := "head-a"
		if calls >= 2 {
			head = "head-b"
		}
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head, Mergeability: gitops.MergeabilityChecking},
			Threads: []gitops.ReviewThread{{
				ID:   "thread-1",
				Path: "main.go",
				Side: "right",
			}},
		}, nil
	}
	events := capturePRUpdates(t, app)

	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	if sub.PRKey != prUpdateKey(testPR) {
		t.Fatalf("subscription PRKey = %q, want %q", sub.PRKey, prUpdateKey(testPR))
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdates.wg.Wait()
	}()

	evt := awaitPRUpdate(t, events, "changed snapshot emit")
	if evt.PRKey != sub.PRKey || evt.HeadSHA != "head-b" || evt.Error != "" {
		t.Fatalf("event = %+v", evt)
	}
	expectNoPRUpdate(t, events, "unchanged snapshot")
}

// TestPRUpdatePumpIsSharedPerPRKey pins the refcount: a pull request is one
// entity, so N callers share ONE poller, ONE change-detection state, and get
// ONE wire event per change — not N pollers spawning N gh/glab processes and
// N copies of the same event.
func TestPRUpdatePumpIsSharedPerPRKey(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var changed atomic.Bool
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		head := "head-a"
		if changed.Load() {
			head = "head-b"
		}
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head}}, nil
	}
	events := capturePRUpdates(t, app)

	subA, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	subB, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	if subA.ID == subB.ID {
		t.Fatalf("expected distinct subscription handles")
	}
	if subA.PRKey != subB.PRKey {
		t.Fatalf("same PR resolved to different keys: %q vs %q", subA.PRKey, subB.PRKey)
	}
	app.prUpdates.mu.Lock()
	pumpCount := len(app.prUpdates.pumps)
	app.prUpdates.mu.Unlock()
	if pumpCount != 1 {
		t.Fatalf("pump count = %d, want 1 for one PR", pumpCount)
	}
	if refs, active, _, present := prPumpState(app, subA.PRKey); !present || refs != 2 || active != 2 {
		t.Fatalf("pump refs=%d active=%d present=%v, want 2/2/true", refs, active, present)
	}

	changed.Store(true)
	evt := awaitPRUpdate(t, events, "shared emit")
	if evt.HeadSHA != "head-b" {
		t.Fatalf("event = %+v", evt)
	}
	// One change, one event: the pre-refactor per-subscription pump emitted
	// one copy per caller on the same channel.
	expectNoPRUpdate(t, events, "duplicate emit for the second subscriber")

	// Releasing one caller leaves the pump running for the other.
	if err := app.UnsubscribePRUpdates(subA.ID); err != nil {
		t.Fatalf("unsubscribe A: %v", err)
	}
	if refs, active, _, present := prPumpState(app, subA.PRKey); !present || refs != 1 || active != 1 {
		t.Fatalf("after releasing A: refs=%d active=%d present=%v, want 1/1/true", refs, active, present)
	}
	app.prUpdates.mu.Lock()
	pump := app.prUpdates.pumps[subA.PRKey]
	app.prUpdates.mu.Unlock()
	if err := app.UnsubscribePRUpdates(subB.ID); err != nil {
		t.Fatalf("unsubscribe B: %v", err)
	}
	if _, _, _, present := prPumpState(app, subA.PRKey); present {
		t.Fatalf("pump survived its last subscriber")
	}
	// The last release stamps the pump dead under the store lock, so a poll
	// it still has in flight cannot stamp state after a replacement exists.
	app.prUpdates.mu.Lock()
	dead := pump.dead
	app.prUpdates.mu.Unlock()
	if !dead {
		t.Fatalf("released pump was not stamped dead")
	}
	app.prUpdates.wg.Wait()
}

// TestSubscribePRUpdatesJoinerDoesNotFetch: one PR is one poll stream AND
// one snapshot. A joiner that fetched its own doubled the gh/glab traffic
// for every second pane, and — worse — could be handed an observation
// nobody else on that pump has, which is exactly the divergence the shared
// pump exists to prevent.
func TestSubscribePRUpdatesJoinerDoesNotFetch(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	var fetches atomic.Int32
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		fetches.Add(1)
		return prUpdateSnapshot{Detail: gitops.PRDetail{
			Number:  got.Number,
			HeadSHA: fmt.Sprintf("head-%d", fetches.Load()),
		}}, nil
	}

	first, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("the pump-creating subscribe made %d fetches, want 1", got)
	}

	second, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("a joiner fetched: %d forge fetches total, want 1", got)
	}
	// The fetch stub answers differently every call, so an identical head
	// here can only mean the joiner took the pump's snapshot.
	if second.HeadSHA != first.HeadSHA {
		t.Fatalf("joiner headSHA = %q, want the pump's %q", second.HeadSHA, first.HeadSHA)
	}

	for _, id := range []string{first.ID, second.ID} {
		if err := app.UnsubscribePRUpdates(id); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
	}
	app.prUpdates.wg.Wait()
}

// TestCreatePRUpdatePumpReconcilesAConcurrentPump: the fetch on the
// pump-creating path runs without the lock, so another caller can register
// the pump in between. The loser must JOIN it and take ITS snapshot —
// keeping the one it fetched would put two subscribers of one pump on two
// different observations, however narrowly they raced.
func TestCreatePRUpdatePumpReconcilesAConcurrentPump(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}

	winner, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// The loser finished its own fetch a moment later; the pump is already
	// there when it takes the lock.
	loser := prUpdateSnapshot{Detail: gitops.PRDetail{Number: testPR.Number, HeadSHA: "head-from-the-loser"}}
	encoded, err := encodePRUpdateSnapshot(loser)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ref, start, err := app.createPRUpdatePump(testPR, winner.PRKey, loser, encoded)
	if err != nil {
		t.Fatalf("createPRUpdatePump: %v", err)
	}
	if start != nil {
		t.Fatal("createPRUpdatePump started a second pump for one PR")
	}
	if ref.snapshot.Detail.HeadSHA != "head-a" {
		t.Fatalf("loser kept its own snapshot (%q), want the live pump's", ref.snapshot.Detail.HeadSHA)
	}
	if ref.seq != winner.Seq {
		t.Fatalf("loser's seq = %d, want the live pump's %d", ref.seq, winner.Seq)
	}
	if refs, active, _, present := prPumpState(app, winner.PRKey); !present || refs != 2 || active != 2 {
		t.Fatalf("pump refs=%d active=%d present=%v, want 2/2/true", refs, active, present)
	}

	for _, handle := range []string{ref.id, winner.ID} {
		if err := app.UnsubscribePRUpdates(handle); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
	}
	app.prUpdates.wg.Wait()
}

func TestSubscribePRUpdatesReleasesOnConnectionClose(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	// Mimics the per-connection ctx the transport layer installs: when the
	// WS connection ends, RunCleanups releases whatever it held.
	ctx, state := transport.WithConnState(context.Background())
	sub, err := app.SubscribePRUpdates(ctx, testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, _, _, present := prPumpState(app, sub.PRKey); !present {
		t.Fatalf("pump should be tracked after Subscribe")
	}

	state.RunCleanups()

	if _, _, _, present := prPumpState(app, sub.PRKey); present {
		t.Fatalf("connection cleanup did not release subscription %q", sub.ID)
	}
	// Idempotent: an explicit unsubscribe afterwards is a no-op.
	if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
		t.Fatalf("Unsubscribe after connection cleanup: %v", err)
	}
	app.prUpdates.wg.Wait()
}

func TestPRUpdatePollingPausesWhileInactiveAndCatchesUpOnResume(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var calls atomic.Int32
	var changed atomic.Bool
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls.Add(1)
		head := "head-a"
		if changed.Load() {
			head = "head-b"
		}
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head, Mergeability: gitops.MergeabilityChecking},
		}, nil
	}
	events := capturePRUpdates(t, app)

	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdates.wg.Wait()
	}()

	if err := app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("SetPRUpdatesActive(false): %v", err)
	}
	// A tick already past the paused check may still complete one fetch;
	// let it drain, then require the count to hold across many intervals.
	time.Sleep(15 * time.Millisecond)
	before := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if after := calls.Load(); after != before {
		t.Fatalf("paused pump kept polling: calls %d -> %d", before, after)
	}

	changed.Store(true)
	if err := app.SetPRUpdatesActive(sub.ID, true); err != nil {
		t.Fatalf("SetPRUpdatesActive(true): %v", err)
	}
	if evt := awaitPRUpdate(t, events, "catch-up poll after resume"); evt.HeadSHA != "head-b" {
		t.Fatalf("event = %+v", evt)
	}
}

// TestSetPRUpdatesActiveComposesAcrossSubscribers drives the whole
// on/off matrix for a SHARED pump. Visibility is per subscriber but the
// pump is per PR, so it must poll while ANY subscriber is active and only
// stop once every one of them has gone quiet — and a repeated report in
// the same direction must not shift the count either way.
func TestSetPRUpdatesActiveComposesAcrossSubscribers(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var calls atomic.Int32
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls.Add(1)
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	polls := func() bool {
		time.Sleep(15 * time.Millisecond)
		before := calls.Load()
		time.Sleep(40 * time.Millisecond)
		return calls.Load() != before
	}

	subA, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	subB, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	defer func() {
		_ = app.UnsubscribePRUpdates(subA.ID)
		_ = app.UnsubscribePRUpdates(subB.ID)
		app.prUpdates.wg.Wait()
	}()

	// One hidden client must not stop the pump for the visible one.
	if err := app.SetPRUpdatesActive(subA.ID, false); err != nil {
		t.Fatalf("A inactive: %v", err)
	}
	if _, active, paused, _ := prPumpState(app, subA.PRKey); active != 1 || paused {
		t.Fatalf("after A hidden: active=%d paused=%v, want 1/false", active, paused)
	}
	if !polls() {
		t.Fatal("pump paused while a subscriber was still active")
	}

	// Repeating the same report is idempotent: the second call must not
	// decrement a vote A already spent.
	if err := app.SetPRUpdatesActive(subA.ID, false); err != nil {
		t.Fatalf("A inactive twice: %v", err)
	}
	if _, active, paused, _ := prPumpState(app, subA.PRKey); active != 1 || paused {
		t.Fatalf("after A hidden twice: active=%d paused=%v, want 1/false", active, paused)
	}

	// Every subscriber hidden: now it stops.
	if err := app.SetPRUpdatesActive(subB.ID, false); err != nil {
		t.Fatalf("B inactive: %v", err)
	}
	if _, active, paused, _ := prPumpState(app, subA.PRKey); active != 0 || !paused {
		t.Fatalf("after both hidden: active=%d paused=%v, want 0/true", active, paused)
	}
	if polls() {
		t.Fatal("pump kept polling with every subscriber hidden")
	}

	// off -> on for the subscriber that went quiet first resumes it.
	if err := app.SetPRUpdatesActive(subA.ID, true); err != nil {
		t.Fatalf("A active again: %v", err)
	}
	if _, active, paused, _ := prPumpState(app, subA.PRKey); active != 1 || paused {
		t.Fatalf("after A visible again: active=%d paused=%v, want 1/false", active, paused)
	}
	if !polls() {
		t.Fatal("pump stayed paused after a subscriber became visible again")
	}

	// A subscriber that leaves while ACTIVE releases its vote: the pump
	// pauses because the only one left is hidden.
	if err := app.UnsubscribePRUpdates(subA.ID); err != nil {
		t.Fatalf("unsubscribe A: %v", err)
	}
	if _, active, paused, present := prPumpState(app, subB.PRKey); !present || active != 0 || !paused {
		t.Fatalf("after active subscriber left: active=%d paused=%v present=%v, want 0/true/true", active, paused, present)
	}
	if polls() {
		t.Fatal("pump kept polling for a hidden subscriber after the visible one left")
	}
}

// TestSubscribingToAPausedPumpWakesIt covers the transition the
// compose-across-subscribers matrix above does not: the resume is caused by
// a NEW subscriber rather than by an existing one's visibility flip. Both
// unpause the pump, so both owe it the same nudge — without one, the pane
// that just opened waits out a full poll interval (45s in production)
// staring at whatever the last visible client saw.
func TestSubscribingToAPausedPumpWakesIt(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 400 * time.Millisecond
	var changed atomic.Bool
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		head := "head-a"
		if changed.Load() {
			head = "head-b"
		}
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head}}, nil
	}
	events := capturePRUpdates(t, app)

	subA, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	if err := app.SetPRUpdatesActive(subA.ID, false); err != nil {
		t.Fatalf("A hidden: %v", err)
	}
	// Let a tick elapse while paused so there is a catch-up poll owed.
	time.Sleep(500 * time.Millisecond)

	changed.Store(true)
	woke := time.Now()
	subB, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe B onto a paused pump: %v", err)
	}
	defer func() {
		_ = app.UnsubscribePRUpdates(subA.ID)
		_ = app.UnsubscribePRUpdates(subB.ID)
		app.prUpdates.wg.Wait()
	}()

	evt := awaitPRUpdate(t, events, "catch-up poll after a new subscriber unpaused the pump")
	if evt.HeadSHA != "head-b" {
		t.Fatalf("event = %+v", evt)
	}
	if waited := time.Since(woke); waited > 200*time.Millisecond {
		t.Fatalf("new subscriber waited %v for the next tick instead of waking the pump", waited)
	}
}

// TestSubscribePRUpdatesRefusesADyingPump: a pump whose goroutine has begun
// tearing down polls nothing ever again, so a caller must not be handed a
// reference on it — it would get a subscription that never emits and a
// visibility vote nothing reads. The fresh pump takes the map entry, and the
// dying one's own drop releases exactly the handles that referenced IT.
func TestSubscribePRUpdatesRefusesADyingPump(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}

	first, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Stand in for the goroutine's teardown having begun: the pump is
	// stamped dead but its drop has not removed it from the map yet.
	app.prUpdates.mu.Lock()
	dying := app.prUpdates.pumps[first.PRKey]
	if dying == nil {
		app.prUpdates.mu.Unlock()
		t.Fatalf("no pump tracked for %q after Subscribe", first.PRKey)
	}
	dying.dead = true
	app.prUpdates.mu.Unlock()

	second, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe onto a dying pump: %v", err)
	}

	app.prUpdates.mu.Lock()
	fresh := app.prUpdates.pumps[first.PRKey]
	held := app.prUpdates.handles[second.ID]
	app.prUpdates.mu.Unlock()
	if fresh == dying {
		t.Fatalf("Subscribe shared the dying pump instead of minting a fresh one")
	}
	if held == nil || held.pump != fresh {
		t.Fatalf("the new handle does not reference the fresh pump")
	}

	// The dying pump's own drop must take only what belonged to IT.
	app.dropPRUpdatePump(dying)

	app.prUpdates.mu.Lock()
	stillMapped := app.prUpdates.pumps[first.PRKey]
	stillHeld, ok := app.prUpdates.handles[second.ID]
	_, staleHeld := app.prUpdates.handles[first.ID]
	app.prUpdates.mu.Unlock()
	if stillMapped != fresh {
		t.Fatalf("the dying pump's drop evicted its successor")
	}
	if !ok || stillHeld.pump != fresh {
		t.Fatalf("the dying pump's drop released a handle it did not own")
	}
	if staleHeld {
		t.Fatalf("the dying pump's own handle survived its drop")
	}

	if err := app.UnsubscribePRUpdates(second.ID); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if _, _, _, present := prPumpState(app, first.PRKey); present {
		t.Fatalf("fresh pump survived its last unsubscribe")
	}
	// The dying pump's goroutine is still parked (the test faked its death
	// rather than letting the lifetime context end it); stop it so Wait
	// cannot block on a fixture artifact.
	close(dying.done)
	app.prUpdates.wg.Wait()
}

// TestPollPRUpdateStoresNothingOnADeadPump: per PR key, sequence order has
// to equal content order — the frontend ranks frames against the seq its
// subscribe returned and drops anything at or below it. A detached pump's
// in-flight poll started BEFORE its replacement's, so a stamp taken after
// the replacement exists would let an older observation outrank a newer one.
// Both critical sections therefore refuse a dead pump: no store, no stamp,
// no emit — which is what keeps successive pumps' live windows from
// overlapping for a key.
func TestPollPRUpdateStoresNothingOnADeadPump(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	var fetchErr error
	head := "head-a"
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		if fetchErr != nil {
			return prUpdateSnapshot{}, fetchErr
		}
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head}}, nil
	}

	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	app.prUpdates.mu.Lock()
	pump := app.prUpdates.pumps[sub.PRKey]
	app.prUpdates.mu.Unlock()
	if pump == nil {
		t.Fatalf("no pump tracked for %q after Subscribe", sub.PRKey)
	}

	// Stand in for the goroutine's teardown having begun while this poll's
	// fetch was already out on the wire.
	app.prUpdates.mu.Lock()
	pump.dead = true
	globalSeq := app.prUpdates.seq
	pumpSeq := pump.seq
	last := string(pump.last)
	app.prUpdates.mu.Unlock()

	assertUnstamped := func(phase string) {
		t.Helper()
		app.prUpdates.mu.Lock()
		defer app.prUpdates.mu.Unlock()
		if app.prUpdates.seq != globalSeq {
			t.Fatalf("%s: global seq moved %d -> %d", phase, globalSeq, app.prUpdates.seq)
		}
		if pump.seq != pumpSeq {
			t.Fatalf("%s: pump seq moved %d -> %d", phase, pumpSeq, pump.seq)
		}
		if string(pump.last) != last {
			t.Fatalf("%s: dead pump stored a snapshot: %s", phase, pump.last)
		}
		if pump.lastErr != "" || pump.lastWireErr != "" {
			t.Fatalf("%s: dead pump stored an error: %q / %q", phase, pump.lastErr, pump.lastWireErr)
		}
	}

	// A snapshot that WOULD have been a change on a live pump.
	head = "head-b"
	if evt, changed := app.pollPRUpdate(pump); changed {
		t.Fatalf("dead pump published a snapshot change: %+v", evt)
	}
	assertUnstamped("snapshot path")

	// And a failure, which stamps through a second critical section.
	fetchErr = errors.New("gh: boom")
	if evt, changed := app.pollPRUpdate(pump); changed {
		t.Fatalf("dead pump published a failure: %+v", evt)
	}
	assertUnstamped("error path")

	if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	app.prUpdates.wg.Wait()
}

// TestSubscribePRUpdatesCapsOutstandingHandles: every distinct PR behind a
// handle costs a goroutine that spawns gh/glab every tick, so the handle map
// is bounded rather than trusting callers to unsubscribe. The refusal is
// typed — retrying the same call never fixes it.
func TestSubscribePRUpdatesCapsOutstandingHandles(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = time.Hour
	var fetches atomic.Int32
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		fetches.Add(1)
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	ids := make([]string, 0, maxPRUpdateHandles)
	defer func() {
		for _, id := range ids {
			_ = app.UnsubscribePRUpdates(id)
		}
		app.prUpdates.wg.Wait()
	}()

	for i := 0; i < maxPRUpdateHandles; i++ {
		sub, err := app.SubscribePRUpdates(context.Background(), testPR)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		ids = append(ids, sub.ID)
	}
	if _, err := app.SubscribePRUpdates(context.Background(), testPR); !errors.Is(err, ErrTooManyPRUpdateSubscriptions) {
		t.Fatalf("subscribe past the cap = %v, want ErrTooManyPRUpdateSubscriptions", err)
	}

	// The refusal costs nothing. A cold PR at the cap must be turned away
	// before the forge fetch, not after: the fetch is the expensive part
	// (a gh/glab process per call), so checking capacity afterwards would
	// let a capped caller keep paying full price on every retry.
	fresh := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 10}
	before := fetches.Load()
	if _, err := app.SubscribePRUpdates(context.Background(), fresh); !errors.Is(err, ErrTooManyPRUpdateSubscriptions) {
		t.Fatalf("cold subscribe past the cap = %v, want ErrTooManyPRUpdateSubscriptions", err)
	}
	if got := fetches.Load() - before; got != 0 {
		t.Fatalf("a refused subscribe ran %d forge fetches, want 0", got)
	}

	// Releasing one makes room again — the cap bounds what is HELD, not how
	// many subscriptions a session may make over its lifetime.
	if err := app.UnsubscribePRUpdates(ids[0]); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	ids = ids[1:]
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe after releasing one: %v", err)
	}
	ids = append(ids, sub.ID)
}

func TestPRUpdateResumeWithoutMissedTickDoesNotPoll(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 300 * time.Millisecond
	var calls atomic.Int32
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls.Add(1)
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a", Mergeability: gitops.MergeabilityChecking},
		}, nil
	}

	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdates.wg.Wait()
	}()

	// Quick hide/show flip well inside one interval: no tick was missed,
	// so resume must not spend a fetch.
	if err := app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("SetPRUpdatesActive(false): %v", err)
	}
	if err := app.SetPRUpdatesActive(sub.ID, true); err != nil {
		t.Fatalf("SetPRUpdatesActive(true): %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("resume without missed tick polled: calls = %d (want 1, the subscribe fetch)", got)
	}
}

// TestPRUpdateFetchFailureSurfacesOnTheEvent pins the doctrine: a forge that
// stops answering is state the reviewer must see on the PR, not a line in a
// log nobody reads. It is emitted once per distinct failure (a poller
// hammering a dead `gh` must not spray the wire) and cleared by the first
// recovery, even when the recovered snapshot is byte-identical to the last
// one broadcast.
func TestPRUpdateFetchFailureSurfacesOnTheEvent(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var failing atomic.Bool
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		if failing.Load() {
			return prUpdateSnapshot{}, errors.New("gh: could not reach github.com")
		}
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	events := capturePRUpdates(t, app)

	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		_ = app.UnsubscribePRUpdates(sub.ID)
		app.prUpdates.wg.Wait()
	}()

	failing.Store(true)
	evt := awaitPRUpdate(t, events, "fetch failure emit")
	if evt.PRKey != sub.PRKey || evt.Error == "" {
		t.Fatalf("failure event = %+v", evt)
	}
	// The forge CLI's own stderr never reaches the wire: it is whatever gh
	// or glab chose to print — remote URLs, a token echoed back by a failed
	// auth call, local clone paths — and this frame goes to every
	// subscriber of the PR. Only a caller-safe summary plus the correlation
	// id that finds the full text in the server log.
	if strings.Contains(evt.Error, "could not reach github.com") {
		t.Fatalf("raw forge stderr reached the wire: %q", evt.Error)
	}
	if !strings.HasPrefix(evt.Error, "failed to refresh pull request (id: ") {
		t.Fatalf("failure message = %q, want the caller-safe summary + correlation id", evt.Error)
	}
	// Same failure every tick — one event, not one per tick.
	expectNoPRUpdate(t, events, "repeated identical failure")

	failing.Store(false)
	recovered := awaitPRUpdate(t, events, "recovery emit")
	if recovered.Error != "" || recovered.HeadSHA != "head-a" {
		t.Fatalf("recovery event = %+v", recovered)
	}
	expectNoPRUpdate(t, events, "unchanged snapshot after recovery")
}

// TestPRUpdateJoinCarriesTheActivePumpError closes the joiner's blind spot:
// identical failures are deduped, so a subscriber that arrives during an
// outage gets NO frame of its own. Without the pump's active failure on the
// subscribe result it would render the pump's stale snapshot with no banner
// until the forge recovered or started failing differently.
func TestPRUpdateJoinCarriesTheActivePumpError(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var failing atomic.Bool
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		if failing.Load() {
			return prUpdateSnapshot{}, errors.New("gh: could not reach github.com")
		}
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	events := capturePRUpdates(t, app)

	first, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if first.Error != "" {
		t.Fatalf("healthy pump handed a joiner an error: %q", first.Error)
	}

	failing.Store(true)
	evt := awaitPRUpdate(t, events, "fetch failure emit")
	joined, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe during outage: %v", err)
	}
	// The same string the subscribers already here are showing, so two panes
	// on one PR cannot disagree about whether it is failing.
	if joined.Error != evt.Error {
		t.Fatalf("joined error = %q, want the emitted %q", joined.Error, evt.Error)
	}
	if strings.Contains(joined.Error, "could not reach github.com") {
		t.Fatalf("raw forge stderr reached the wire: %q", joined.Error)
	}
	// The failure does not blank what the pump last observed.
	if joined.HeadSHA != "head-a" {
		t.Fatalf("joined headSHA = %q, want the pump's last good observation", joined.HeadSHA)
	}

	failing.Store(false)
	if recovered := awaitPRUpdate(t, events, "recovery emit"); recovered.Error != "" {
		t.Fatalf("recovery event = %+v", recovered)
	}
	third, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe after recovery: %v", err)
	}
	if third.Error != "" {
		t.Fatalf("a joiner after recovery carries a stale error: %q", third.Error)
	}

	for _, id := range []string{first.ID, joined.ID, third.ID} {
		if err := app.UnsubscribePRUpdates(id); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
	}
	app.prUpdates.wg.Wait()
}

// TestPRUpdateJoinCarriesThePumpSequence pins the ordering a subscriber
// needs to close the join→push handoff race: it reads the pump's state under
// the mutex, but its alias for "pr:updated" is only installed once the
// response gets home. A frame emitted in that window routes nowhere, and the
// pump does not re-emit until the PR moves again. The sequence is what lets
// the subscriber replay exactly the frames its reference does not already
// account for.
func TestPRUpdateJoinCarriesThePumpSequence(t *testing.T) {
	app := NewApp()
	app.prUpdates.interval = 5 * time.Millisecond
	var head atomic.Value
	head.Store("head-a")
	app.prUpdates.fetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{
			Number:  got.Number,
			HeadSHA: head.Load().(string),
		}}, nil
	}
	events := capturePRUpdates(t, app)

	first, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if first.Seq == 0 {
		t.Fatal("a fresh pump handed out seq 0, which orders below every frame")
	}

	head.Store("head-b")
	changed := awaitPRUpdate(t, events, "changed snapshot emit")
	if changed.Seq <= first.Seq {
		t.Fatalf("frame seq %d does not outrank the reference it postdates (%d)", changed.Seq, first.Seq)
	}

	// A joiner is handed the state that emit published, stamped with the
	// same sequence — so replaying that frame over it would be a no-op, and
	// the guard skips it.
	joined, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if joined.Seq != changed.Seq {
		t.Fatalf("joined seq = %d, want the emitted frame's %d", joined.Seq, changed.Seq)
	}
	if joined.HeadSHA != "head-b" {
		t.Fatalf("joined headSHA = %q, want head-b", joined.HeadSHA)
	}

	head.Store("head-c")
	later := awaitPRUpdate(t, events, "second change emit")
	if later.Seq <= joined.Seq {
		t.Fatalf("frame seq %d does not outrank the join it postdates (%d)", later.Seq, joined.Seq)
	}

	for _, id := range []string{first.ID, joined.ID} {
		if err := app.UnsubscribePRUpdates(id); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
	}
	app.prUpdates.wg.Wait()
}

func TestSetPRUpdatesActiveUnknownIDIsNoOp(t *testing.T) {
	app := NewApp()
	if err := app.SetPRUpdatesActive("nope", true); err != nil {
		t.Fatalf("SetPRUpdatesActive: %v", err)
	}
}

func TestPRUpdateKeyMatchesTheFrontendSourceKey(t *testing.T) {
	// The frontend's PR sourceKey is "pr:" + this key; one spelling of
	// "which PR" on both sides of the wire.
	cases := []struct {
		pr   gitops.PRReference
		want string
	}{
		{gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 5}, "github:owner/repo:5"},
		{gitops.PRReference{Forge: "gitlab", Namespace: "group/sub", Repo: "repo", Number: 12}, "gitlab:group/sub/repo:12"},
	}
	for _, tt := range cases {
		if got := prUpdateKey(tt.pr); got != tt.want {
			t.Fatalf("prUpdateKey(%+v) = %q, want %q", tt.pr, got, tt.want)
		}
	}
}
