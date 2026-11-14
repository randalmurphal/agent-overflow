package accountswitch

import (
	"context"
	"slices"
	"time"

	"agent-overflow/internal/provider"
)

// originWindow is how long after the selection moved a refusal of a turn
// that was running can belong to an account the turn used before: a request
// in flight at the switch can still be refused on it. A usage-limit refusal
// answers a request when it is made, so this bounds the answer's arrival.
const originWindow = 2 * time.Minute

type refusal struct {
	provider, threadID, model string
	at                        time.Time
	// giveUp is set when the thread stops waiting: a continue sent after a
	// guessed wait was refused again on a guess.
	giveUp bool
	// reread lists the accounts the refusal can belong to when it marked
	// none of them. They are read again before the decision.
	reread []string
}

// movedTurn is a turn that was running when the selection moved.
type movedTurn struct {
	// accounts are the ones the turn used before each move.
	accounts []string
	// at is when the selection last moved under the turn.
	at time.Time
}

// SelectionMoved tells the coordinator the provider's selected account
// changed from one account to another, by any switch. When running sessions
// follow a switch, each turn then in flight is remembered as having used
// from, for TurnRefused. It may be called from inside Deps.Switch.
func (c *Coordinator) SelectionMoved(prov, fromAccountID, toAccountID string) {
	if fromAccountID == "" || fromAccountID == toAccountID || !c.deps.ReachesRunningTurns(prov) {
		return
	}
	inflight := c.deps.InFlight(prov)
	now := c.deps.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, turn := range c.movedTurns {
		if now.Sub(turn.at) > originWindow {
			delete(c.movedTurns, id)
		}
	}
	for _, t := range inflight {
		turn := c.movedTurns[t.ThreadID]
		if !slices.Contains(turn.accounts, fromAccountID) {
			turn.accounts = append(turn.accounts, fromAccountID)
		}
		turn.at = now
		c.movedTurns[t.ThreadID] = turn
	}
}

// TurnRefused records a usage-limit refusal of threadID's turn, reported by
// a session using accountID. The refusal mark applies before it returns; the
// forced decision, the notice and continue delivery run in the background.
//
// The provider does not say which account refused a request. A turn that was
// running when the selection moved less than originWindow ago can have been
// refused on any account it used, and more than once (a subagent, then its
// parent). Such a refusal marks no account: the accounts are read again and
// the readings decide. The thread's next send ends that, in BeforeSend.
func (c *Coordinator) TurnRefused(prov, threadID, model, accountID string) {
	cfg := c.deps.Settings()
	now := c.deps.Now()
	_, continuable := c.deps.ContinueModel(threadID)
	wait := cfg.AutoContinue && continuable
	r := refusal{provider: prov, threadID: threadID, model: model, at: now}

	// suspects are the accounts the refused request can have used.
	var suspects []string
	c.mu.Lock()
	if turn, ok := c.movedTurns[threadID]; ok && now.Sub(turn.at) <= originWindow {
		suspects = slices.Clone(turn.accounts)
	}
	c.mu.Unlock()
	if accountID != "" && !slices.Contains(suspects, accountID) {
		suspects = append(suspects, accountID)
	}
	sole := accountID != "" && len(suspects) == 1
	var m mark
	switch {
	case sole:
		var limits []provider.RateLimitEntry
		if snap, ok := c.deps.Reading(prov, accountID); ok {
			limits = snap.Limits
		}
		m = newMark(prov, modelFamily(prov, modelKey(prov, model)), limits, cfg.ThresholdPercent, now)
	case len(suspects) == 0:
		// No account is known, so the wait is the one-hour guess.
		m.guessed = true
	default:
		r.reread = suspects
	}

	c.mu.Lock()
	if sole {
		key := accountKey{prov, accountID}
		c.marks[key] = addMark(c.marks[key], m, now)
	}
	r.giveUp = c.giveUpLocked(threadID, wait, m.guessed, now)
	if wait && !r.giveUp && cfg.Revision >= c.revokedBefore {
		c.gen++
		c.waiting[threadID] = &waiter{provider: prov, gen: c.gen, admitted: cfg.Revision, guessed: m.guessed}
	}
	c.pendingRefusals = append(c.pendingRefusals, r)
	c.mu.Unlock()
	c.signal()
}

// addMark drops expired marks and keeps one mark per family and width.
func addMark(marks []mark, m mark, now time.Time) []mark {
	out := marks[:0]
	for _, old := range marks {
		if !now.Before(old.until) {
			continue
		}
		if old.family == m.family && old.allModels == m.allModels {
			if old.until.After(m.until) {
				m.until = old.until
			}
			continue
		}
		out = append(out, old)
	}
	return append(out, m)
}

func (c *Coordinator) handleRefusal(ctx context.Context, r refusal) {
	if ctx.Err() != nil {
		return
	}
	if len(r.reread) == 0 {
		c.deps.RequestReading(r.provider)
	}
	for _, id := range r.reread {
		// An earlier refusal of the same turn may have read it already.
		if snap, ok := c.deps.Reading(r.provider, id); !ok || snap.UpdatedAt <= r.at.UnixMilli() {
			c.refreshReading(ctx, r.provider, id)
		}
	}
	c.decideOffPath(ctx, r.provider, []string{r.model}, []string{r.threadID})
	if ctx.Err() != nil {
		return
	}
	if r.giveUp {
		c.deps.ThreadNotice(r.threadID, giveUpNotice)
	} else {
		c.noticeIfUnservable(r)
	}
	c.deliver(ctx, r.provider)
}
