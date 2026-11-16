package wsllauncher

import (
	"sync"

	"agent-overflow/internal/webview2host"
)

// directiveLanes runs browser-host directives off the read loop in the order
// they arrived, one lane per page. A handler blocks on the launcher's UI
// thread, and a create first on a cold environment, so directives cannot run
// inline; one goroutine each (the shape of the other directives) let a page's
// bounds, show and hide apply in any order, leaving a view hidden that the
// backend showed or at a place it already left. Lanes of different pages stay
// independent, so one page's slow create delays only that page.
//
// A bounds directive queued behind another bounds of the same page replaces
// it: only the newest place matters, and a wedged UI thread cannot grow a
// lane by one directive per frame.
type directiveLanes struct {
	run func(webview2host.Directive)

	mu sync.Mutex
	// lanes holds each busy lane's waiting directives. A key is present
	// while its drain runs, even with nothing left waiting.
	lanes map[string][]webview2host.Directive
}

func newDirectiveLanes(run func(webview2host.Directive)) *directiveLanes {
	return &directiveLanes{run: run, lanes: make(map[string][]webview2host.Directive)}
}

// laneKey is the page a directive addresses; directives that address none
// (accelerators, close-profile) get lanes of their own.
func laneKey(d webview2host.Directive) string {
	switch {
	case d.PageID != "":
		return "page:" + d.PageID
	case d.ProfileID != "":
		return "profile:" + d.ProfileID
	default:
		return "op:" + string(d.Op)
	}
}

// submit queues a directive on its lane and starts the lane's drain when it
// is idle. It never blocks on a handler.
func (l *directiveLanes) submit(d webview2host.Directive) {
	key := laneKey(d)
	l.mu.Lock()
	waiting, busy := l.lanes[key]
	if n := len(waiting); n > 0 && d.Op == webview2host.OpBounds && waiting[n-1].Op == webview2host.OpBounds {
		waiting[n-1] = d
	} else {
		waiting = append(waiting, d)
	}
	l.lanes[key] = waiting
	l.mu.Unlock()
	if !busy {
		go l.drain(key)
	}
}

func (l *directiveLanes) drain(key string) {
	for {
		l.mu.Lock()
		waiting := l.lanes[key]
		if len(waiting) == 0 {
			delete(l.lanes, key)
			l.mu.Unlock()
			return
		}
		next := waiting[0]
		l.lanes[key] = waiting[1:]
		l.mu.Unlock()
		l.run(next)
	}
}
