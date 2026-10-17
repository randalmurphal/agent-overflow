package app

import "sync"

// workGroup counts goroutines as a sync.WaitGroup does, for work that
// starts at any time and from any goroutine: an emit, a provider event, a
// settlement. A WaitGroup requires every Add that leaves zero to happen
// before the Wait it might race, and panics when one lands while a Wait is
// returning; that work cannot promise it while shutdown, the harness or a
// test waits. Wait returns at the first moment no member runs, and a member
// that starts after that moment is not waited for. The zero value is ready.
type workGroup struct {
	mu   sync.Mutex
	idle sync.Cond
	n    int
	// zeros counts the moments n reached zero, so a Wait wakes at the
	// first one even when a member has started again since.
	zeros uint64
}

func (g *workGroup) Add(delta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n += delta
	if g.n < 0 {
		panic("app: workGroup counter below zero")
	}
	if g.n == 0 && delta < 0 {
		g.zeros++
		g.idle.Broadcast()
	}
}

func (g *workGroup) Done() { g.Add(-1) }

func (g *workGroup) Wait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idle.L == nil {
		g.idle.L = &g.mu
	}
	for start := g.zeros; g.n > 0 && g.zeros == start; {
		g.idle.Wait()
	}
}
