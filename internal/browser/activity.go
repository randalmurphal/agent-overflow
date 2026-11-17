package browser

import (
	"context"
	"sync"
	"time"
)

// Agent activity. A tab shows that an agent is using its page while a
// browser tool call acts on it. The MCP server marks each tool call's
// context (withAgentCall); lockPageForOperation, which every page operation
// takes the page through, enters the call on the page it locked, and the
// call leaves every page it entered when it returns. The state is pushed on
// transitions only: the first call in shows the activity, and it clears
// agentLinger after the last call out unless another call enters first, so
// an agent's run of calls is one transition each way. Nothing is gated on
// it: the user's own actions on the page run as they always do.

// agentActivityLinger is how long a page keeps showing activity after the
// last tool call on it, so consecutive calls do not flicker the tab.
const agentActivityLinger = 1500 * time.Millisecond

// pageActivity is a page's agent activity. Guarded by its mu.
type pageActivity struct {
	mu sync.Mutex
	// calls counts the tool calls inside the page.
	calls int
	// shown is what the companion state says.
	shown bool
	// clearGen identifies the pending clear; a call entering or a newer
	// clear makes an older one a no-op.
	clearGen uint64
	timer    *time.Timer
}

// agentCall is one tool call's activity: the pages it entered, and the
// Manager that owns them.
type agentCall struct {
	mu    sync.Mutex
	m     *Manager
	pages []*managedPage
	done  bool
}

type agentCallKey struct{}

// withAgentCall marks ctx as an agent's tool call. end leaves every page the
// call entered; call it once the call has answered.
func withAgentCall(ctx context.Context) (context.Context, func()) {
	call := &agentCall{}
	return context.WithValue(ctx, agentCallKey{}, call), func() {
		call.mu.Lock()
		m, pages := call.m, call.pages
		call.pages, call.done = nil, true
		call.mu.Unlock()
		for _, p := range pages {
			m.leaveAgentCall(p)
		}
	}
}

// noteAgentOperation enters ctx's tool call, when it is one, on p, once per
// call. A user action carries no call and changes nothing.
func (m *Manager) noteAgentOperation(ctx context.Context, p *managedPage) {
	call, _ := ctx.Value(agentCallKey{}).(*agentCall)
	if call == nil {
		return
	}
	call.mu.Lock()
	if call.done {
		call.mu.Unlock()
		return
	}
	for _, entered := range call.pages {
		if entered == p {
			call.mu.Unlock()
			return
		}
	}
	call.m = m
	call.pages = append(call.pages, p)
	call.mu.Unlock()

	a := &p.activity
	a.mu.Lock()
	a.calls++
	a.clearGen++
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	changed := !a.shown
	a.shown = true
	a.mu.Unlock()
	if changed {
		m.emitThreadState(p.owner)
	}
}

// leaveAgentCall ends one call's activity on p. The last one out schedules
// the clear.
func (m *Manager) leaveAgentCall(p *managedPage) {
	a := &p.activity
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls--
	if a.calls > 0 {
		return
	}
	a.clearGen++
	gen := a.clearGen
	a.timer = time.AfterFunc(m.agentLinger, func() { m.clearAgentActivity(p, gen) })
}

// clearAgentActivity is a scheduled clear: it applies unless a call entered
// or another clear was scheduled since, which a timer that fired as a call
// entered and stopped it finds.
func (m *Manager) clearAgentActivity(p *managedPage, gen uint64) {
	a := &p.activity
	a.mu.Lock()
	if a.clearGen != gen {
		a.mu.Unlock()
		return
	}
	a.shown, a.timer = false, nil
	a.mu.Unlock()
	m.emitThreadState(p.owner)
}

// agentActive is what the page's tab shows.
func (p *managedPage) agentActive() bool {
	p.activity.mu.Lock()
	defer p.activity.mu.Unlock()
	return p.activity.shown
}
