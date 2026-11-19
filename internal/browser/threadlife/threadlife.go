// Package threadlife keeps the lifetimes of threads' browsers that pane
// actions are bound to. An action binds to its thread's current lifetime
// before anything it does can wait (Registry.Bind), and each change it
// commits is admitted only while that lifetime holds (Binding.Admits). A
// retirement (Registry.Retire) ends the lifetime for every action bound to
// it; an action that binds after it gets a new one. A lifetime is kept only
// while an action is bound to it, so the registry holds no entry for a
// thread nothing acts on.
//
// The registry's lock is innermost: a caller may hold its own lock across
// any call, and its commit and a retirement made under that lock are then
// ordered by it.
package threadlife

import (
	"errors"
	"sync"
)

// ThreadID names a thread.
type ThreadID string

// ErrRetired answers an action whose thread's lifetime was retired while it
// was bound.
var ErrRetired = errors.New("browser: the thread's pages were closed while the action ran")

// Registry is the threads' current lifetimes. The zero value is ready.
type Registry struct {
	mu    sync.Mutex
	lives map[ThreadID]*life
}

type life struct {
	thread  ThreadID
	retired bool
	bound   int
}

// Binding is one action's hold on its thread's lifetime. Its End is called
// once, when the action is done; a nil Binding admits everything.
type Binding struct {
	registry *Registry
	life     *life
	once     sync.Once
}

// Bind binds an action to the thread's current lifetime.
func (r *Registry) Bind(thread ThreadID) *Binding {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lives == nil {
		r.lives = make(map[ThreadID]*life)
	}
	l := r.lives[thread]
	if l == nil {
		l = &life{thread: thread}
		r.lives[thread] = l
	}
	l.bound++
	return &Binding{registry: r, life: l}
}

// Retire ends the thread's current lifetime: every action bound to it is
// refused from here (Admits), and the next Bind starts a new one.
func (r *Registry) Retire(thread ThreadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lives[thread]; l != nil {
		l.retired = true
		delete(r.lives, thread)
	}
}

// Admits answers ErrRetired once the lifetime b is bound to was retired, for
// a change to thread; a binding to another thread, or none, admits it.
func (b *Binding) Admits(thread ThreadID) error {
	if b == nil || b.life.thread != thread {
		return nil
	}
	b.registry.mu.Lock()
	defer b.registry.mu.Unlock()
	if b.life.retired {
		return ErrRetired
	}
	return nil
}

// End releases the binding, once however often it is called, and answers
// ErrRetired when its lifetime was retired while it was bound.
func (b *Binding) End() error {
	r := b.registry
	b.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		b.life.bound--
		if b.life.bound == 0 && r.lives[b.life.thread] == b.life {
			delete(r.lives, b.life.thread)
		}
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if b.life.retired {
		return ErrRetired
	}
	return nil
}

// Len is how many lifetimes the registry holds.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lives)
}
