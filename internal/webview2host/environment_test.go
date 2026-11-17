package webview2host

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeEnv struct{ id int }

// envRig drives a paneEnvironment with one UI goroutine running begin,
// completed and release in order. Each start is
// reported so the test decides when and how it completes. beforeDispatch,
// when set, runs once between an arm and its dispatch to the UI thread,
// which is where a clear-data can slip in.
type envRig struct {
	t      *testing.T
	env    *paneEnvironment[*fakeEnv]
	ui     chan func()
	starts chan uint64

	mu             sync.Mutex
	freed          []*fakeEnv
	beforeDispatch func()
}

func newEnvRig(t *testing.T, timeout time.Duration) *envRig {
	t.Helper()
	r := &envRig{t: t, ui: make(chan func()), starts: make(chan uint64, 8)}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case fn := <-r.ui:
				fn()
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop) })
	r.env = newPaneEnvironment(
		func(gen uint64) { r.starts <- gen },
		func(e *fakeEnv) {
			r.mu.Lock()
			r.freed = append(r.freed, e)
			r.mu.Unlock()
		},
		t.Logf, timeout)
	return r
}

func (r *envRig) do(fn func()) {
	done := make(chan struct{})
	r.ui <- func() { defer close(done); fn() }
	<-done
}

func (r *envRig) nextStart() uint64 {
	r.t.Helper()
	select {
	case gen := <-r.starts:
		return gen
	case <-time.After(5 * time.Second):
		r.t.Fatal("no environment create started")
		return 0
	}
}

func (r *envRig) noStart() {
	r.t.Helper()
	select {
	case gen := <-r.starts:
		r.t.Fatalf("an environment create started for generation %d", gen)
	case <-time.After(20 * time.Millisecond):
	}
}

func (r *envRig) freedList() []*fakeEnv {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeEnv(nil), r.freed...)
}

type acquired struct {
	env *fakeEnv
	err error
}

func (r *envRig) acquireAsync() <-chan acquired {
	out := make(chan acquired, 1)
	go func() {
		request, start := r.env.request()
		if start {
			r.mu.Lock()
			hook := r.beforeDispatch
			r.beforeDispatch = nil
			r.mu.Unlock()
			if hook != nil {
				hook()
			}
			r.do(func() { r.env.begin(request.gen) })
		}
		env, err := r.env.await(request)
		out <- acquired{env, err}
	}()
	return out
}

func wait(t *testing.T, ch <-chan acquired) acquired {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("acquire never returned")
		return acquired{}
	}
}

// A clear-data that runs between a create's arm and the dispatch of its
// start ends the generation that start was armed for. The start must not
// create under the next generation: the next create arms that one, and two
// creates for one generation let the obsolete one's failure settle it for
// good while the other's environment is adopted.
func TestPaneEnvironmentSkipsAStartAClearSuperseded(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	r.beforeDispatch = func() { r.do(r.env.release) }
	if got := r.acquireAsync(); !errors.Is(wait(t, got).err, errEnvironmentReleased) {
		t.Fatal("the create parked on the cleared generation was not told to retry")
	}
	r.noStart()

	pending := r.acquireAsync()
	gen := r.nextStart()
	if gen != 1 {
		t.Fatalf("the create after the clear started generation %d, want 1", gen)
	}
	r.noStart()
	e := &fakeEnv{id: 1}
	r.do(func() { r.env.completed(gen, e, nil) })
	if got := wait(t, pending); got.err != nil || got.env != e {
		t.Fatalf("the create after the clear got %v, %v", got.env, got.err)
	}
	if got := wait(t, r.acquireAsync()); got.err != nil || got.env != e {
		t.Fatalf("a later create got %v, %v; want the adopted environment", got.env, got.err)
	}
}

// A completion for a generation a clear ended frees its environment instead
// of adopting it, and the next create starts a fresh one.
func TestPaneEnvironmentFreesALateCompletionOfAClearedGeneration(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	pending := r.acquireAsync()
	gen := r.nextStart()
	start := time.Now()
	r.do(r.env.release)
	if got := wait(t, pending); !errors.Is(got.err, errEnvironmentReleased) {
		t.Fatalf("the parked create got %v, want the retry error", got.err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the parked create sat out its timeout")
	}
	late := &fakeEnv{id: 1}
	r.do(func() { r.env.completed(gen, late, nil) })
	if freed := r.freedList(); len(freed) != 1 || freed[0] != late {
		t.Fatalf("freed %v, want the late environment", freed)
	}
	if _, ok := r.env.current(); ok {
		t.Fatal("a late completion of a cleared generation was adopted")
	}

	pending = r.acquireAsync()
	next := r.nextStart()
	fresh := &fakeEnv{id: 2}
	r.do(func() { r.env.completed(next, fresh, nil) })
	if got := wait(t, pending); got.err != nil || got.env != fresh {
		t.Fatalf("the next create got %v, %v", got.env, got.err)
	}
}

// A failed create settles its generation for every waiter; a clear re-arms
// it and the adopted environment of the next generation is freed by the
// following clear.
func TestPaneEnvironmentFailureSettlesUntilAClear(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	pending := r.acquireAsync()
	gen := r.nextStart()
	r.do(func() { r.env.completed(gen, nil, errors.New("no runtime")) })
	if got := wait(t, pending); got.err == nil || got.err.Error() != "no runtime" {
		t.Fatalf("create got %v, want the settled failure", got.err)
	}
	if got := wait(t, r.acquireAsync()); got.err == nil {
		t.Fatal("a later create of the failed generation succeeded")
	}
	r.noStart()

	r.do(r.env.release)
	pending = r.acquireAsync()
	next := r.nextStart()
	e := &fakeEnv{id: 1}
	r.do(func() { r.env.completed(next, e, nil) })
	if got := wait(t, pending); got.err != nil || got.env != e {
		t.Fatalf("retry got %v, %v", got.env, got.err)
	}
	r.do(r.env.release)
	if freed := r.freedList(); len(freed) != 1 || freed[0] != e {
		t.Fatalf("freed %v, want the adopted environment once", freed)
	}
}
