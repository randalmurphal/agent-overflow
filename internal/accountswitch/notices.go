package accountswitch

import (
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/provider"
)

// noticeMemoryAge bounds how long a thread that never sends again keeps
// its last unservable notice.
const noticeMemoryAge = 24 * time.Hour

// noticeMemo is the last unservable notice written to a thread.
type noticeMemo struct {
	text string
	at   time.Time
}

// giveUpNotice tells a thread it stopped waiting after repeated refusals
// whose reset time was guessed.
const giveUpNotice = "Could not tell when this usage limit resets. Send a message to continue this thread."

func label(a Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

func limitName(e provider.RateLimitEntry) string {
	if e.LimitName != "" {
		return e.LimitName
	}
	return e.LimitID
}

func providerTitle(prov string) string {
	switch prov {
	case providerClaude:
		return "Claude"
	case providerCodex:
		return "Codex"
	}
	return prov
}

func switchNotice(prov string, d decision) string {
	if d.reason == ReasonOrder {
		return fmt.Sprintf("Switched %s account to %s, the first available in the auto-switch order.", providerTitle(prov), label(d.to))
	}
	var cause string
	switch {
	case d.reason == ReasonRefused && d.model != "":
		cause = fmt.Sprintf("%s reached a usage limit for %s", label(d.from), d.model)
	case d.reason == ReasonRefused || !d.hasLimit:
		cause = fmt.Sprintf("%s reached a usage limit", label(d.from))
	default:
		cause = fmt.Sprintf("%s is at %d%% of its %s limit", label(d.from), int(math.Round(d.percent)), limitName(d.limit))
	}
	return fmt.Sprintf("Switched %s account from %s to %s: %s.", providerTitle(prov), label(d.from), label(d.to), cause)
}

func unservableNotice(hasPool bool, at time.Time, who Account, known, waiting bool, now time.Time) string {
	when := at.In(now.Location()).Format("Mon 3:04 PM")
	var text string
	switch {
	case hasPool && known:
		text = fmt.Sprintf("No account in the auto-switch pool can run this model right now. The earliest reset is %s at %s.", label(who), when)
	case hasPool:
		text = "No account in the auto-switch pool can run this model right now."
	case known:
		text = fmt.Sprintf("This account is out of usage for this model until %s.", when)
	default:
		text = "This account is out of usage for this model."
	}
	switch {
	case waiting && known:
		text += " This thread continues then."
	case waiting:
		text += " This thread continues when the limit resets."
	}
	return text
}

func continueFailedNotice(err error) string {
	return "Could not continue this thread after the usage limit: " + err.Error()
}

// noticeIfUnservable tells the refused thread when no account can serve its
// model, with the earliest time one can. Without a pool it speaks only to a
// waiting thread, so a single account with auto-continue off sees nothing new.
func (c *Coordinator) noticeIfUnservable(r refusal) {
	cfg := c.deps.Settings()
	now := c.deps.Now()
	pool := c.deps.Pool(r.provider)
	family := modelFamily(r.provider, modelKey(r.provider, r.model))
	cands := c.candidates(r.provider, pool)
	c.mu.Lock()
	_, waiting := c.waiting[r.threadID]
	c.mu.Unlock()

	var selected *candidate
	var poolCands []candidate
	usableOthers := 0
	for i, cand := range cands {
		if cand.ID == pool.SelectedID {
			selected = &cands[i]
		}
		if cand.InPool && cand.usable {
			poolCands = append(poolCands, cand)
			if cand.ID != pool.SelectedID {
				usableOthers++
			}
		}
	}
	if selected == nil {
		return
	}
	hasPool := selected.InPool && usableOthers > 0
	if !hasPool && !waiting {
		return
	}
	accounts := []candidate{*selected}
	if hasPool {
		accounts = poolCands
	}
	for _, a := range accounts {
		if assess(r.provider, family, a.limits, a.marks, cfg.ThresholdPercent, now, nil).state != stateExhausted {
			return
		}
	}
	at, who, known := earliestServe(r.provider, family, accounts, cfg.ThresholdPercent, now)
	text := unservableNotice(hasPool, at, who, known, waiting, now)
	// One turn can be refused more than once (a subagent, then its parent).
	c.mu.Lock()
	repeated := c.unservableNoticed[r.threadID].text == text
	for id, memo := range c.unservableNoticed {
		if now.Sub(memo.at) > noticeMemoryAge {
			delete(c.unservableNoticed, id)
		}
	}
	c.unservableNoticed[r.threadID] = noticeMemo{text: text, at: now}
	c.mu.Unlock()
	if !repeated {
		c.deps.ThreadNotice(r.threadID, text)
	}
}

func reportFor(prov string, d decision) Report {
	r := Report{
		Provider:      prov,
		FromAccountID: d.from.ID,
		ToAccountID:   d.to.ID,
		FromLabel:     label(d.from),
		ToLabel:       label(d.to),
		Reason:        d.reason,
		Model:         d.model,
	}
	if d.hasLimit {
		r.LimitName = limitName(d.limit)
		r.UsedPercent = d.percent
		r.ResetsAt = d.limit.ResetsAt
	}
	return r
}
