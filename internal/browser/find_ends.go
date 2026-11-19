package browser

import (
	"context"
	"sync"
)

// How a page's search ends outside a find action (find.go): an end raised
// where waiting is not allowed is submitted to the page (findEnds) and
// applied by its next transition, or by a transition of its own.

// findEnds are the ends submitted to a page and not yet applied. Bounded:
// a commit's end keeps only the newest epoch, and a presentation end
// rereads what is shown, so one of each is enough. Guarded by its own mu,
// which nothing holds while waiting.
type findEnds struct {
	mu      sync.Mutex
	epoch   uint64
	review  bool
	all     bool
	running bool
}

// endLocked ends p's search and answers the end as its owner should see
// it. The caller holds p.mu and p.findMu.
func (m *Manager) endLocked(p *managedPage) FindState {
	p.find.gen = m.findGen.Add(1)
	state := p.find.identify(p, FindState{Current: -1})
	p.find.disown()
	return state
}

// settleFindLocked applies the ends submitted to p, announces the end, and
// makes a stop the engine is owed. A page that is no longer live ends its
// search and asks its closed driver nothing. The caller holds p.mu.
func (m *Manager) settleFindLocked(ctx context.Context, p *managedPage) error {
	p.ends.mu.Lock()
	epoch, review, all := p.ends.epoch, p.ends.review, p.ends.all
	p.ends.epoch, p.ends.review, p.ends.all = 0, false, false
	p.ends.mu.Unlock()
	live := m.pageLive(p)
	p.findMu.Lock()
	ends := false
	if p.find.active {
		switch {
		case !live || all:
			ends = true
		case epoch != 0 && p.find.doc < epoch:
			ends = true
		case review:
			ends = !m.pageShown(p.owner.id, p.id) || !m.mountOf(p.owner.id, p.find.owner)
		}
	}
	var state FindState
	if ends {
		state = m.endLocked(p)
	}
	owed := live && !p.find.active && !p.find.settled()
	p.findMu.Unlock()
	if ends {
		m.emit(CompanionEvent{Kind: "find", ThreadID: string(p.owner.id), PageID: string(p.id), Find: &state})
	}
	if !owed {
		return nil
	}
	finder, mode := pageFinder(p)
	if mode == "" {
		return nil
	}
	return m.clearEngineFind(ctx, p, finder)
}

// endFindLocked ends p's search at once, for a caller closing or suspending
// the page under its lock. Those callers close the driver first, so the
// settle asks the engine nothing; it is bounded like every other one. The
// caller holds p.mu.
func (m *Manager) endFindLocked(p *managedPage) {
	p.ends.mu.Lock()
	p.ends.all = true
	p.ends.mu.Unlock()
	ctx, cancel := operationContext(context.Background(), p.ctx, m.findTimeout)
	defer cancel()
	_ = m.settleFindLocked(ctx, p)
}

// submitFindEnd hands p an end to apply, without waiting: the running
// transition applies it when it finishes, or one of its own does. Safe on
// any thread, the engine's UI thread included. Once Close has begun to join
// the find work, an end is no longer applied: the browser's close ended
// every search before it.
func (m *Manager) submitFindEnd(p *managedPage, set func(*findEnds)) {
	p.ends.mu.Lock()
	set(&p.ends)
	start := !p.ends.running && m.admitFindWork()
	if start {
		p.ends.running = true
	}
	p.ends.mu.Unlock()
	if start {
		go m.applyFindEnds(p)
	}
}

// admitFindWork counts one goroutine of find work for Close to join, or
// answers false once Close has begun joining it.
func (m *Manager) admitFindWork() bool {
	m.findGate.Lock()
	defer m.findGate.Unlock()
	if m.findClosed {
		return false
	}
	m.findWork.Add(1)
	return true
}

// applyFindEnds is a transition of its own for submitted ends, repeated
// until none is left.
func (m *Manager) applyFindEnds(p *managedPage) {
	defer m.findWork.Done()
	for {
		p.mu.Lock()
		ctx, cancel := operationContext(context.Background(), p.ctx, m.findTimeout)
		m.findPoint("apply")
		_ = m.settleFindLocked(ctx, p)
		cancel()
		p.mu.Unlock()
		p.ends.mu.Lock()
		if p.ends.epoch == 0 && !p.ends.review && !p.ends.all {
			p.ends.running = false
			p.ends.mu.Unlock()
			return
		}
		p.ends.mu.Unlock()
	}
}

// endFind ends p's search on the Manager's own account: the page closed
// without its lock held by the caller, or the browser closed.
func (m *Manager) endFind(p *managedPage) {
	m.submitFindEnd(p, func(e *findEnds) { e.all = true })
}

// documentCommitted is pageHooks.DocumentCommitted. It runs where the engine
// reports the commit and only stamps the new document epoch, so an action
// that reads the epoch after the commit is bound to the new document. The
// work it returns submits the end of a search started on an earlier
// document; one started on this document or a later one is not its to end.
func (m *Manager) documentCommitted(p *managedPage) func() {
	epoch := p.docEpoch.Add(1)
	return func() {
		m.submitFindEnd(p, func(e *findEnds) { e.epoch = max(e.epoch, epoch) })
	}
}

// endHiddenFinds submits to each of the thread's live pages that holds a
// search or owes the engine a stop, or is in a transition, an end of a
// search not on the shown active page or whose owning pane mount is gone,
// and with it the owed stop. What is shown is read when the end is applied.
// A settled page outside a transition is skipped: a transition starting
// after the change reads it in its own checks.
func (m *Manager) endHiddenFinds(pages []*managedPage) {
	for _, p := range pages {
		p.findMu.Lock()
		settled := p.find.settled()
		p.findMu.Unlock()
		if settled && !p.findBusy.Load() {
			continue
		}
		m.submitFindEnd(p, func(e *findEnds) { e.review = true })
	}
}
