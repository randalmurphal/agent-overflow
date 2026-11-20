package app

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A spawner's computer from before this release gives back a prompt its
// thread did not take now, not only one no thread there can hold, and says
// neither. While the worker's prompts are that thread's, such a return
// keeps the prompt the spawner's here and offers it again later, telling
// that computer to read once it is listed.

// fakePromptClock is a promptClock a test moves by hand.
type fakePromptClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakePromptTimer]struct{}
}

type fakePromptTimer struct {
	at time.Time
	f  func()
}

// usePromptClock gives app's prompt registry a clock the test moves.
func usePromptClock(app *App) *fakePromptClock {
	clock := &fakePromptClock{now: time.Now(), timers: map[*fakePromptTimer]struct{}{}}
	w := &app.workerPrompts
	w.mu.Lock()
	w.clock = clock
	w.mu.Unlock()
	return clock
}

func (c *fakePromptClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakePromptClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakePromptTimer{at: c.now.Add(d), f: f}
	c.timers[timer] = struct{}{}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, armed := c.timers[timer]
		delete(c.timers, timer)
		return armed
	}
}

// advance moves the clock by d and runs the timers due by then.
func (c *fakePromptClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for timer := range c.timers {
		if !timer.at.After(c.now) {
			due = append(due, timer.f)
			delete(c.timers, timer)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// armed is the waits of the timers still to run, from now.
func (c *fakePromptClock) armed() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for timer := range c.timers {
		out = append(out, timer.at.Sub(c.now))
	}
	return out
}

// An older spawner's return keeps the prompt its spawner's: the worker
// reads waiting on it, the person is not asked, and the prompt is listed
// again under a new token once the wait runs out, each wait twice the
// last up to the bound, armed once per return. Returned while it is no
// longer the spawner's, it goes to the person.
func TestAnOlderSpawnersReturnOffersThePromptAgainLater(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	clock := usePromptClock(f.app)
	worker := f.workers[0].ID
	token := f.ask(t, 0)
	for i := range 7 {
		if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{Spawn: f.spawns[0], Token: token}); err != nil {
			t.Fatalf("the older spawner's return: %v", err)
		}
		f.drain(t)
		if route := f.route(t, worker); route == nil || route.ThreadID != f.lead || f.handedToPerson(worker) {
			t.Fatalf("after a return the prompt is routed to %+v, want it still the lead's on Desk", route)
		}
		if waiting := f.app.threadWaitingOn(worker); waiting.routedTo() != "the lead on Desk" {
			t.Fatalf("after a return the worker reads routed to %q, want the lead on Desk", waiting.routedTo())
		}
		want := min(peerPromptOfferFirst<<i, peerPromptOfferMost)
		if armed := clock.armed(); len(armed) != 1 || armed[0] != want {
			t.Fatalf("after return %d the armed waits = %v; want one of %v", i+1, armed, want)
		}
		clock.advance(want - time.Second)
		if listed := f.list(t, 0); len(listed) != 0 {
			t.Fatalf("before the wait ran out the spawner's listing = %+v, want nothing", listed)
		}
		clock.advance(time.Second)
		listed := f.list(t, 0)
		if len(listed) != 1 || listed[0].Token == token {
			t.Fatalf("once the wait ran out the spawner's listing = %+v, want the prompt under a new token", listed)
		}
		token = listed[0].Token
	}

	// With a sidebar row and no run of the spawner's open, a return gives
	// it to the person here.
	f.endSpawnRun(t, 0)
	if _, err := f.app.store.SetThreadPlacement([]string{worker}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if err := f.app.offerPeerPromptAgain(token); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if route := f.route(t, worker); route != nil || !f.handedToPerson(worker) {
		t.Fatalf("returned once it is no longer the spawner's, the prompt is routed to %+v, want the person here", route)
	}
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a prompt handed to the person left waits %v armed", armed)
	}
}

// A prompt offered again that closes before its wait runs out stops its
// timer: an answer in the worker or by its spawner, a return with no
// holder, a placement that moves it, and the app's shutdown each leave
// nothing armed, and a timer that runs anyway tells nothing of a
// registration that closed.
func TestAPromptOfferedAgainThatClosesFirstArmsNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		// question asks async questions, whose answer lands in the
		// worker's queue, instead of an approval.
		question bool
		end      func(t *testing.T, f *linkedWorkers, offered string)
	}{
		"answered in the worker": {end: func(t *testing.T, f *linkedWorkers, _ string) {
			resolveApprovalOn(t, f.app, f.workers[0].ID, "approval-1")
		}},
		"answered by its spawner": {question: true, end: func(t *testing.T, f *linkedWorkers, offered string) {
			if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReply, threadPromptPeerArgs{
				Spawn: f.spawns[0], Token: offered, Answers: []string{"One", "tonight"},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		"returned with no holder": {end: func(t *testing.T, f *linkedWorkers, offered string) {
			if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{Spawn: f.spawns[0], Token: offered, Reason: peerPromptNoHolder}); err != nil {
				t.Fatal(err)
			}
		}},
		"moved by its placement": {end: func(t *testing.T, f *linkedWorkers, _ string) {
			f.endSpawnRun(t, 0)
			f.place(t, 0, store.PlacementStandalone)
		}},
		"shut down": {end: func(t *testing.T, f *linkedWorkers, _ string) {
			if err := f.app.closeWorkerPrompts(t.Context(), 10*time.Second); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkedWorkers(t, 1)
			clock := usePromptClock(f.app)
			var token string
			if c.question {
				f.recordQuestions(t, 0)
				listed := f.list(t, 0)
				if len(listed) != 1 {
					t.Fatalf("the link lists %+v, want the questions", listed)
				}
				token = listed[0].Token
			} else {
				token = f.ask(t, 0)
			}
			if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{Spawn: f.spawns[0], Token: token}); err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			if armed := clock.armed(); len(armed) != 1 {
				t.Fatalf("after the return the armed waits = %v, want one", armed)
			}
			w := &f.app.workerPrompts
			w.mu.Lock()
			var offered string
			for candidate, prompt := range w.byToken {
				if prompt.remote != nil && prompt.state == workerPromptRouted {
					offered = candidate
				}
			}
			w.mu.Unlock()
			c.end(t, f, offered)
			waitUntil(t, 10*time.Second, func() bool { return len(clock.armed()) == 0 })
			f.drain(t)

			// A timer that ran anyway, for the closed registration, tells
			// the spawner's computer nothing.
			activity := &f.app.ownerActivity
			activity.mu.Lock()
			activity.devices = map[string]*deviceActivity{f.device: {entries: map[string]ownerEntry{}, changed: make(chan struct{})}}
			activity.mu.Unlock()
			w.offerDue(offered)
			activity.mu.Lock()
			told := activity.devices[f.device].entries
			activity.mu.Unlock()
			if len(told) != 0 {
				t.Fatalf("a closed registration's timer told %v", told)
			}
		})
	}
}

// Across two computers whose spawner's computer reads by the owner
// changes it is told of, and returns with no reason as one before this
// release does: the prompt of a worker whose spawner there can no longer
// hold it is offered again at each wait's end, that computer reads the
// new token then with nothing else to make it read, and gives it back
// again. Only the worker's clock moves; the person there is never asked.
func TestAPromptAnOlderSpawnerKeepsGivingBackIsOfferedAgainAndStaysItsSpawners(t *testing.T) {
	t.Parallel()
	p, peer := promptOwnerOpPair(t)
	clock := usePromptClock(p.dest)
	peer.reasonless.Store(true)
	if _, _, err := p.source.store.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	// The worker's approval is held for its spawner, which that computer is
	// told of and gives back.
	startReconciledOwner(t, p.reachPair)
	for returns := int32(1); returns <= 3; returns++ {
		waitUntil(t, 15*time.Second, func() bool { return peer.returns.Load() >= returns && len(clock.armed()) == 1 })
		if got := peer.returns.Load(); got != returns {
			t.Fatalf("the spawner's computer gave the prompt back %d times, want %d", got, returns)
		}
		if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID {
			t.Fatalf("after %d returns the prompt there is routed to %+v (open %v), want its spawner here", returns, route, open)
		}
		if p.announcedToPerson("req-allow") {
			t.Fatalf("after %d returns the person there was asked", returns)
		}
		if returns < 3 {
			clock.advance(clock.armed()[0])
		}
	}
}
