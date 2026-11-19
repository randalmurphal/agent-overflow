package browser

import (
	"fmt"

	"agent-overflow/internal/browser/ids"
)

// Read-only probes of hostedPages for tests.

// running counts the downloads that run.
func (p *hostedPages) running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.records {
		n += len(r.downloads)
	}
	return n
}

// waiting answers the kinds and downloads of the work waiting on page's
// lane, in order.
func (p *hostedPages) waiting(page ids.PageHandle) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	if lane := p.lanes.pages[page]; lane != nil {
		for _, w := range lane.jobs {
			out = append(out, fmt.Sprintf("%d %s %d", w.kind, w.snapshot.ID, w.snapshot.Received))
		}
	}
	return out
}

// laneOf answers the key of the lane page's work runs on now, and false
// when it has none.
func (p *hostedPages) laneOf(page ids.PageHandle) (ids.PageHandle, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lane := p.lanes.pages[page]; lane != nil {
		return lane.key, true
	}
	return "", false
}

// busy reports whether any lane has work.
func (p *hostedPages) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lanes.byKey) != 0
}

// state answers page's state, and false when it has no record.
func (p *hostedPages) state(page ids.PageHandle) (hostedPageState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r != nil {
		return r.state, true
	}
	return 0, false
}

// cancelsOf answers the last cancel attempt sent for download id of page.
func (p *hostedPages) cancelsOf(page ids.PageHandle, id downloadID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.records[page]; r != nil && r.downloads[id] != nil {
		return int(r.downloads[id].cancels)
	}
	return -1
}

// endedOf counts page's kept ended ids, and runs whether id runs there.
func (p *hostedPages) endedOf(page ids.PageHandle, id downloadID) (ended int, runs bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil {
		return 0, false
	}
	return len(r.ended), r.downloads[id] != nil
}

// watchingNow reports whether the status loop runs.
func (p *hostedPages) watchingNow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watching
}
