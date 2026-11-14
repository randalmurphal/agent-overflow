package accountswitch

import (
	"context"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

const (
	// ContinueMessage is the message queued to a thread a usage limit stopped.
	ContinueMessage = "Continue. The usage limit that stopped this thread no longer applies."

	// resetGrace is how long after a reset the continue check accepts it
	// without a reading taken after it.
	resetGrace = 5 * time.Minute

	// guessedRetryWindow is how soon after a continue queued on a guessed
	// wait a refusal on a guess ends the waiting instead of guessing again.
	guessedRetryWindow = 2 * time.Minute
)

// waiter is a thread to continue once its model can run again. The model is
// read from the thread at each check, since it can change while waiting.
type waiter struct {
	provider string
	gen      uint64
	// admitted is the settings revision the wait was admitted at.
	admitted uint64
	// guessed is set when the refusal's mark had no known reset time.
	guessed bool
	// delivering is set while Continue runs so the next check skips it.
	delivering bool
}

// giveUpLocked reports whether a refusal of threadID ends its waiting: it
// is a guessed refusal within guessedRetryWindow of a continue queued after
// a guessed wait. Any refusal consumes that memory.
func (c *Coordinator) giveUpLocked(threadID string, wait, guessed bool, now time.Time) bool {
	at, ok := c.guessedContinues[threadID]
	delete(c.guessedContinues, threadID)
	return ok && wait && guessed && now.Sub(at) <= guessedRetryWindow
}

func (c *Coordinator) hasWaiting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiting) > 0
}

// checkWaiting is the periodic check: a forced decision with the waiting
// threads' models in the demand, then continue delivery.
func (c *Coordinator) checkWaiting(ctx context.Context) {
	c.mu.Lock()
	providers := make(map[string]string, len(c.waiting))
	for id, w := range c.waiting {
		providers[id] = w.provider
	}
	c.mu.Unlock()
	byProvider := map[string][]string{}
	for id, prov := range providers {
		models := byProvider[prov]
		if model, ok := c.deps.ContinueModel(id); ok {
			models = append(models, model)
		}
		byProvider[prov] = models
	}
	for prov, models := range byProvider {
		c.decideOffPath(ctx, prov, models, nil)
		c.deliver(ctx, prov)
	}
}

// continueVerdict is what the continue check says about one waiting thread.
type continueVerdict int

const (
	continueNow continueVerdict = iota
	// continueLater keeps the wait: the selected account is exhausted for
	// the thread's model, or auto-continue is off and the write that turned
	// it off has not revoked the wait yet.
	continueLater
	// continueNever ends the wait: the thread cannot receive a continue, or
	// its provider has no selected account to serve it.
	continueNever
)

// continueCheck is a provider's selected account as the continue check sees
// it at one moment.
type continueCheck struct {
	provider  string
	accountID string
	limits    []provider.RateLimitEntry
	readAt    int64 // Unix milliseconds; 0 without a reading
	marks     []mark
	threshold int
	// autoContinue is the setting.
	autoContinue bool
	now          time.Time
	// needReading is set when a passed reset waits for a reading to confirm it.
	needReading bool
}

func (c *Coordinator) newContinueCheck(prov string) *continueCheck {
	cfg := c.deps.Settings()
	k := &continueCheck{
		provider:     prov,
		accountID:    c.deps.Pool(prov).SelectedID,
		threshold:    cfg.ThresholdPercent,
		autoContinue: cfg.AutoContinue,
		now:          c.deps.Now(),
	}
	if k.accountID == "" {
		return k
	}
	if snap, ok := c.deps.Reading(prov, k.accountID); ok {
		k.limits, k.readAt = snap.Limits, snap.UpdatedAt
	}
	c.mu.Lock()
	k.marks = append([]mark(nil), c.marks[accountKey{prov, k.accountID}]...)
	c.mu.Unlock()
	return k
}

// verdict takes Deps.ContinueModel's result for a waiting thread. A passed
// reset counts once a reading taken after it exists or resetGrace has passed.
func (k *continueCheck) verdict(model string, continuable bool) continueVerdict {
	if k.accountID == "" || !continuable {
		return continueNever
	}
	if !k.autoContinue {
		return continueLater
	}
	confirm := func(at time.Time) bool {
		if k.readAt > at.UnixMilli() || !k.now.Before(at.Add(resetGrace)) {
			return true
		}
		k.needReading = true
		return false
	}
	family := modelFamily(k.provider, modelKey(k.provider, model))
	if assess(k.provider, family, k.limits, k.marks, k.threshold, k.now, confirm).state == stateExhausted {
		return continueLater
	}
	return continueNow
}

// deliver starts a continue for each waiting thread of prov the continue
// check passes, and requests a reading when a passed reset needs one.
func (c *Coordinator) deliver(ctx context.Context, prov string) {
	if ctx.Err() != nil {
		return
	}
	type pendingWaiter struct {
		threadID string
		gen      uint64
		guessed  bool
	}
	var ws []pendingWaiter
	c.mu.Lock()
	for id, w := range c.waiting {
		if w.provider == prov && !w.delivering {
			ws = append(ws, pendingWaiter{id, w.gen, w.guessed})
		}
	}
	c.mu.Unlock()
	if len(ws) == 0 {
		return
	}
	k := c.newContinueCheck(prov)
	for _, w := range ws {
		switch k.verdict(c.deps.ContinueModel(w.threadID)) {
		case continueNever:
			c.dropWaiter(w.threadID, w.gen)
		case continueNow:
			if !c.beginDelivery(w.threadID, w.gen) {
				continue
			}
			c.deliveries.Add(1)
			go func() {
				defer c.deliveries.Done()
				c.continueThread(ctx, prov, w.threadID, w.gen, w.guessed)
			}()
		}
	}
	if k.needReading {
		c.deps.RequestReading(prov)
	}
}

// continueThread runs one Continue. A continue that was not admitted, or
// that timed out, keeps the wait for the next check; cancellation ends
// quietly; any other failure drops the wait and tells the thread.
func (c *Coordinator) continueThread(ctx context.Context, prov, threadID string, gen uint64, guessed bool) {
	queued := false
	err := c.deps.Continue(ctx, threadID, func() bool {
		queued = c.admit(prov, threadID, gen, guessed)
		return queued
	})
	if err == nil {
		if queued {
			c.dropWaiter(threadID, gen)
		} else {
			c.endDelivery(threadID, gen)
		}
		return
	}
	// The continue was not queued, so it is not a retry to bound.
	c.mu.Lock()
	delete(c.guessedContinues, threadID)
	c.mu.Unlock()
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		c.endDelivery(threadID, gen)
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("accountswitch: continue thread %s: %v; retrying at the next check", threadID, err)
		c.endDelivery(threadID, gen)
	default:
		c.dropWaiter(threadID, gen)
		log.Printf("accountswitch: continue thread %s: %v", threadID, err)
		c.deps.ThreadNotice(threadID, continueFailedNotice(err))
	}
}

func (c *Coordinator) beginDelivery(threadID string, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.waiting[threadID]
	if w == nil || w.gen != gen || w.delivering {
		return false
	}
	w.delivering = true
	return true
}

func (c *Coordinator) endDelivery(threadID string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.waiting[threadID]; w != nil && w.gen == gen {
		w.delivering = false
	}
}

// admit is Continue's callback, run under the thread's action lock. The
// selection, a reading or the thread's model can have changed while the lock
// was taken, so the continue check runs again here, and the wait must still
// be the one the delivery started for. For a guessed wait it records when
// the continue was queued.
func (c *Coordinator) admit(prov, threadID string, gen uint64, guessed bool) bool {
	if c.newContinueCheck(prov).verdict(c.deps.ContinueModel(threadID)) != continueNow {
		return false
	}
	now := c.deps.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.waiting[threadID]
	if w == nil || w.gen != gen {
		return false
	}
	if guessed {
		for id, at := range c.guessedContinues {
			if now.Sub(at) > guessedRetryWindow {
				delete(c.guessedContinues, id)
			}
		}
		c.guessedContinues[threadID] = now
	}
	return true
}

// dropWaiter ends the wait of generation gen.
func (c *Coordinator) dropWaiter(threadID string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.waiting[threadID]; w != nil && w.gen == gen {
		delete(c.waiting, threadID)
	}
}
