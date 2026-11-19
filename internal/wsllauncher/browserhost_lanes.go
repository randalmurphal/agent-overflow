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
// A close-profile runs on a lane of its own but closes every page of its
// profile, so a create for that profile that arrived after it waits until it
// has run: a jar closed and reopened at once reuses its profile id, and the
// replacement page must not be taken by the close that preceded it.
//
// A bounds directive queued behind another bounds of the same page replaces
// it: only the newest place matters, and a wedged UI thread cannot grow a
// lane by one directive per frame.
type directiveLanes struct {
	run func(webview2host.Directive)

	mu sync.Mutex
	// lanes holds each busy lane's waiting directives. A key is present
	// while its drain runs, even with nothing left waiting.
	lanes map[string][]laneItem
	// closing holds, per profile id, the done channel of the newest
	// close-profile that has not finished running.
	closing map[string]chan struct{}
}

type laneItem struct {
	directive webview2host.Directive
	// after, on a create, is closed once the close-profile of its profile
	// that arrived before it has run.
	after <-chan struct{}
	// done, on a close-profile, is closed once it has run.
	done chan struct{}
}

func newDirectiveLanes(run func(webview2host.Directive)) *directiveLanes {
	return &directiveLanes{
		run:     run,
		lanes:   make(map[string][]laneItem),
		closing: make(map[string]chan struct{}),
	}
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
	item := laneItem{directive: d}
	l.mu.Lock()
	if d.Op == webview2host.OpCloseProfile {
		item.done = make(chan struct{})
		l.closing[d.ProfileID] = item.done
	}
	if d.Op == webview2host.OpCreate {
		item.after = l.closing[d.ProfileID]
	}
	waiting, busy := l.lanes[key]
	if n := len(waiting); n > 0 && d.Op == webview2host.OpBounds && waiting[n-1].directive.Op == webview2host.OpBounds {
		waiting[n-1] = item
	} else {
		waiting = append(waiting, item)
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
		if next.after != nil {
			<-next.after
		}
		l.run(next.directive)
		if next.done != nil {
			l.mu.Lock()
			if l.closing[next.directive.ProfileID] == next.done {
				delete(l.closing, next.directive.ProfileID)
			}
			l.mu.Unlock()
			close(next.done)
		}
	}
}
