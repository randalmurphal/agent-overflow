package app

import "sync"

// workGroup counts goroutines as a sync.WaitGroup does, for work that
// starts at any time and from any goroutine: an emit, a provider event, a
// settlement. A WaitGroup requires every Add that leaves zero to happen
// before the Wait it might race, and panics when one lands while a Wait is
// returning; that work cannot promise it while shutdown, the harness or a
// test waits. Wait returns at the first moment no member runs, and a member
// that starts after that moment is not waited for. Every member starts
// through Go, which Close refuses, so shutdown joins every member that
// started. The zero value is ready.
type workGroup struct {
	mu   sync.Mutex
	idle sync.Cond
	n    int
	// zeros counts the moments n reached zero, so a Wait wakes at the
	// first one even when a member has started again since.
	zeros  uint64
	closed bool
}

// Go runs fn on its own goroutine as a member and reports true, or runs
// nothing and reports false once Close has begun.
func (g *workGroup) Go(fn func()) bool { return g.GoCounted(nil, fn) }

// GoCounted is Go that also counts the member in counted for its life, so
// a Wait on counted waits for it too. counted is never closed.
func (g *workGroup) GoCounted(counted *workGroup, fn func()) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.n++
	g.mu.Unlock()
	if counted != nil {
		counted.addMember(1)
	}
	go func() {
		defer g.addMember(-1)
		if counted != nil {
			defer counted.addMember(-1)
		}
		fn()
	}()
	return true
}

// Close refuses every later Go and waits for the members running.
func (g *workGroup) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.Wait()
}

// addMember moves the count. Only Go and its members' ends call it: a
// member counted any other way would bypass Close.
func (g *workGroup) addMember(delta int) {
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

// members is how many members run, for tests.
func (g *workGroup) members() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}
