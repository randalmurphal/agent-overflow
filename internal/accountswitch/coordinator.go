package accountswitch

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	// staleReading is the age after which an off-send-path target is read
	// again before a switch.
	staleReading         = 5 * time.Minute
	refreshTimeout       = 20 * time.Second
	defaultCheckInterval = time.Minute
)

type accountKey struct {
	provider, accountID string
}

// Coordinator applies the auto-switch contract. Its methods are safe for
// concurrent use. Background work runs on one worker goroutine between
// Start and Stop, plus one goroutine per continue being delivered; triggers
// that arrive before Start wait for it.
type Coordinator struct {
	deps     Deps
	interval time.Duration

	mu          sync.Mutex
	marks       map[accountKey][]mark
	needsLogin  map[accountKey]bool
	lastFailure map[string]string // provider -> target and error of the last failed switch
	// switching is set while the coordinator's own Switch call for the
	// provider runs.
	switching map[string]bool
	waiting   map[string]*waiter
	// guessedContinues is when a continue after a guessed wait was queued,
	// by thread.
	guessedContinues map[string]time.Time
	// movedTurns is, by thread, the turns that were running when the
	// selection moved, until the thread's next send.
	movedTurns map[string]movedTurn
	// revokedBefore is the highest revision RevokeWaitsBefore was given.
	revokedBefore uint64
	// unservableNoticed is the last "no account can run this model" notice
	// of each thread since its last send.
	unservableNoticed map[string]noticeMemo
	gen               uint64
	decideMu          map[string]*sync.Mutex

	pendingRefusals []refusal
	pendingReadings map[accountKey]bool
	pendingDeliver  map[string]bool
	pendingCheck    bool

	deliveries sync.WaitGroup
	wake       chan struct{}
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
}

// New returns a coordinator. Call Start to run background work.
func New(deps Deps, opts ...Option) *Coordinator {
	c := &Coordinator{
		deps:              deps,
		interval:          defaultCheckInterval,
		marks:             map[accountKey][]mark{},
		needsLogin:        map[accountKey]bool{},
		lastFailure:       map[string]string{},
		switching:         map[string]bool{},
		waiting:           map[string]*waiter{},
		guessedContinues:  map[string]time.Time{},
		movedTurns:        map[string]movedTurn{},
		unservableNoticed: map[string]noticeMemo{},
		decideMu:          map[string]*sync.Mutex{},
		pendingReadings:   map[accountKey]bool{},
		pendingDeliver:    map[string]bool{},
		wake:              make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Start runs the worker until ctx ends or Stop is called. Only the first
// call has an effect.
func (c *Coordinator) Start(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()
	go c.run(ctx, done)
}

// Stop cancels the worker and the deliveries in progress and waits for them
// to return.
func (c *Coordinator) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	// Only the worker starts deliveries, so none starts after done closes.
	c.deliveries.Wait()
}

func (c *Coordinator) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	var ticker *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	for {
		c.runPending(ctx)
		// The check compares wall-clock times on each tick, so a suspended
		// machine catches up at its next tick.
		if c.hasWaiting() {
			if ticker == nil {
				ticker = time.NewTicker(c.interval)
				tick = ticker.C
			}
		} else if ticker != nil {
			ticker.Stop()
			ticker, tick = nil, nil
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-tick:
			c.mu.Lock()
			c.pendingCheck = true
			c.mu.Unlock()
		}
	}
}

func (c *Coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// runPending processes queued triggers until none remain.
func (c *Coordinator) runPending(ctx context.Context) {
	for ctx.Err() == nil {
		c.mu.Lock()
		refusals := c.pendingRefusals
		readings := c.pendingReadings
		deliver := c.pendingDeliver
		check := c.pendingCheck
		c.pendingRefusals = nil
		c.pendingReadings = map[accountKey]bool{}
		c.pendingDeliver = map[string]bool{}
		c.pendingCheck = false
		c.mu.Unlock()
		if len(refusals) == 0 && len(readings) == 0 && len(deliver) == 0 && !check {
			return
		}
		for _, r := range refusals {
			c.handleRefusal(ctx, r)
		}
		decided := map[string]bool{}
		for k := range readings {
			if decided[k.provider] || k.accountID != c.deps.Pool(k.provider).SelectedID || !c.deps.ReachesRunningTurns(k.provider) {
				continue
			}
			decided[k.provider] = true
			c.decideOffPath(ctx, k.provider, nil, nil)
		}
		if check {
			c.checkWaiting(ctx)
		}
		for prov := range deliver {
			c.deliver(ctx, prov)
		}
	}
}

func (c *Coordinator) providerLock(prov string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.decideMu[prov]
	if m == nil {
		m = &sync.Mutex{}
		c.decideMu[prov] = m
	}
	return m
}

// BeforeSend runs on the send path under the thread's action lock. It ends
// the thread's wait and, when the thread has no turn of its own in flight,
// makes a forced or order switch from cached readings. It makes no
// network call.
func (c *Coordinator) BeforeSend(prov, threadID, model string, ownTurnInFlight bool) {
	c.mu.Lock()
	delete(c.waiting, threadID)
	delete(c.unservableNoticed, threadID)
	c.mu.Unlock()
	if ownTurnInFlight {
		return
	}
	// The turn this send starts runs on the account selected when BeforeSend
	// returns, including one a switch below selects.
	defer func() {
		c.mu.Lock()
		delete(c.movedTurns, threadID)
		c.mu.Unlock()
	}()
	if !c.poolActive(prov, c.deps.Pool(prov)) {
		return
	}
	lock := c.providerLock(prov)
	lock.Lock()
	defer lock.Unlock()
	for range c.attemptLimit(prov) {
		inflight := c.deps.InFlight(prov)
		othersRunning := false
		for _, t := range inflight {
			if t.ThreadID != threadID {
				othersRunning = true
			}
		}
		models := append([]string{model}, threadModels(inflight)...)
		d := c.computeLocked(prov, models, !othersRunning)
		if !d.switching() {
			return
		}
		if !c.switchLocked(prov, d, []string{threadID}) {
			return
		}
	}
}

// poolActive reports whether auto-switch can act for the provider: the
// selected account is in the pool and another usable pool account exists.
func (c *Coordinator) poolActive(prov string, pool Pool) bool {
	selectedInPool := false
	others := false
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range pool.Accounts {
		if !a.InPool {
			continue
		}
		if a.ID == pool.SelectedID {
			selectedInPool = true
		} else if a.Usable && !c.needsLogin[accountKey{prov, a.ID}] {
			others = true
		}
	}
	return selectedInPool && others
}

// ReadingUpdated notes a new reading and returns at once without calling
// any dependency. The worker makes a forced decision when the account is
// the selected one and running sessions follow a switch, and checks the
// waiting threads.
func (c *Coordinator) ReadingUpdated(prov, accountID string) {
	c.mu.Lock()
	c.pendingReadings[accountKey{prov, accountID}] = true
	c.pendingDeliver[prov] = true
	c.mu.Unlock()
	c.signal()
}

// RevokeWaitsBefore ends the waits admitted before a write to the
// auto-continue setting, given that write's revision. A wait admitted while
// the setting was on does not survive it being switched off, however soon it
// is switched on again and in whatever order the two writes are observed.
func (c *Coordinator) RevokeWaitsBefore(revision uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision > c.revokedBefore {
		c.revokedBefore = revision
	}
	for id, w := range c.waiting {
		if w.admitted < revision {
			delete(c.waiting, id)
		}
	}
	clear(c.guessedContinues)
}

// AccountsChanged forgets marks of accounts no longer listed and every
// needs-login result of the provider. The listing change a switch of the
// coordinator's own publishes keeps the needs-login results, so a signed-out
// account is not retried after every switch.
func (c *Coordinator) AccountsChanged(prov string) {
	listed := map[string]bool{}
	for _, a := range c.deps.Pool(prov).Accounts {
		listed[a.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.marks {
		if k.provider == prov && !listed[k.accountID] {
			delete(c.marks, k)
		}
	}
	if c.switching[prov] {
		return
	}
	for k := range c.needsLogin {
		if k.provider == prov {
			delete(c.needsLogin, k)
		}
	}
}

func threadModels(ts []ThreadModel) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Model)
	}
	return out
}

func (c *Coordinator) attemptLimit(prov string) int {
	return 2*len(c.deps.Pool(prov).Accounts) + 2
}

// computeLocked decides from the current listing, readings and marks.
func (c *Coordinator) computeLocked(prov string, models []string, allowOrder bool) decision {
	cfg := c.deps.Settings()
	now := c.deps.Now()
	pool := c.deps.Pool(prov)
	return decide(decideInput{
		provider:   prov,
		selectedID: pool.SelectedID,
		pinned:     pool.Pinned,
		accounts:   c.candidates(prov, pool),
		demand:     buildDemand(prov, models),
		strategy:   cfg.Strategy,
		threshold:  cfg.ThresholdPercent,
		allowOrder: allowOrder,
		now:        now,
	})
}

// candidates pairs the listed accounts with their readings and marks.
func (c *Coordinator) candidates(prov string, pool Pool) []candidate {
	cands := make([]candidate, len(pool.Accounts))
	for i, a := range pool.Accounts {
		cands[i].Account = a
		if snap, ok := c.deps.Reading(prov, a.ID); ok {
			cands[i].limits = snap.Limits
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, a := range pool.Accounts {
		key := accountKey{prov, a.ID}
		cands[i].marks = append([]mark(nil), c.marks[key]...)
		cands[i].usable = a.Usable && !c.needsLogin[key]
	}
	return cands
}

// switchLocked performs d while the provider lock is held. It reports
// whether the caller should decide again (the target needed a sign-in).
func (c *Coordinator) switchLocked(prov string, d decision, noticeThreads []string) bool {
	c.mu.Lock()
	c.switching[prov] = true
	c.mu.Unlock()
	needsLogin, err := c.deps.Switch(prov, d.to.ID)
	c.mu.Lock()
	delete(c.switching, prov)
	c.mu.Unlock()
	if needsLogin {
		c.mu.Lock()
		c.needsLogin[accountKey{prov, d.to.ID}] = true
		c.mu.Unlock()
		return true
	}
	report := reportFor(prov, d)
	if err != nil {
		failure := d.to.ID + "\x00" + err.Error()
		c.mu.Lock()
		repeated := c.lastFailure[prov] == failure
		c.lastFailure[prov] = failure
		c.mu.Unlock()
		if !repeated {
			report.Error = err.Error()
			c.deps.Report(report)
		}
		return false
	}
	c.mu.Lock()
	delete(c.lastFailure, prov)
	c.pendingDeliver[prov] = true
	c.mu.Unlock()
	c.deps.Report(report)
	text := switchNotice(prov, d)
	for _, id := range noticeThreads {
		c.deps.ThreadNotice(id, text)
	}
	c.signal()
	return false
}

// decideOffPath runs a forced decision with models plus the in-flight
// models. A stale target is read again first, at most once per call.
// Notices go to noticeThreads, or to every in-flight thread when it is nil.
func (c *Coordinator) decideOffPath(ctx context.Context, prov string, models []string, noticeThreads []string) {
	refreshed := map[string]bool{}
	lock := c.providerLock(prov)
	for range c.attemptLimit(prov) {
		if ctx.Err() != nil {
			return
		}
		lock.Lock()
		inflight := c.deps.InFlight(prov)
		d := c.computeLocked(prov, append(append([]string(nil), models...), threadModels(inflight)...), false)
		if !d.switching() {
			lock.Unlock()
			return
		}
		if !refreshed[d.to.ID] && c.targetStale(prov, d.to.ID) {
			lock.Unlock()
			refreshed[d.to.ID] = true
			c.refreshReading(ctx, prov, d.to.ID)
			continue
		}
		notices := noticeThreads
		if notices == nil {
			notices = make([]string, 0, len(inflight))
			for _, t := range inflight {
				notices = append(notices, t.ThreadID)
			}
		}
		again := c.switchLocked(prov, d, notices)
		lock.Unlock()
		if !again {
			return
		}
	}
}

// refreshReading reads one account's usage again. A failed read is logged
// and the decision uses the cached reading.
func (c *Coordinator) refreshReading(ctx context.Context, prov, accountID string) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := c.deps.RefreshReading(ctx, prov, accountID); err != nil {
		log.Printf("accountswitch: read %s account %s: %v", prov, accountID, err)
	}
}

func (c *Coordinator) targetStale(prov, accountID string) bool {
	snap, ok := c.deps.Reading(prov, accountID)
	return !ok || c.deps.Now().Sub(time.UnixMilli(snap.UpdatedAt)) > staleReading
}
