package webview2host

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeEnv stands in for a COM environment: free records each release.
type fakeEnv struct{ id int }

// envRig drives a paneEnvironment the way the Windows host does: one UI
// goroutine runs start, completed, release and releaseIdle in order, and
// each start is reported so the test decides when and how it completes.
type envRig struct {
	t      *testing.T
	env    *paneEnvironment[*fakeEnv]
	ui     chan func()
	starts chan uint64

	mu    sync.Mutex
	freed []*fakeEnv
	pages int
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
	r.env = newPaneEnvironment(r.do,
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

func (r *envRig) complete(gen uint64, e *fakeEnv) {
	r.do(func() { r.env.completed(gen, e, nil) })
}

func (r *envRig) releaseIdle() (bool, string) {
	var released bool
	var reason string
	r.do(func() {
		released, reason = r.env.releaseIdle(func() int {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.pages
		})
	})
	return released, reason
}

func (r *envRig) setPages(n int) {
	r.mu.Lock()
	r.pages = n
	r.mu.Unlock()
}

func (r *envRig) freedList() []*fakeEnv {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeEnv(nil), r.freed...)
}

type acquired struct {
	env  *fakeEnv
	done func()
	err  error
}

// acquireAsync runs a create's acquire on its own goroutine, as a directive
// lane does.
func (r *envRig) acquireAsync() <-chan acquired {
	out := make(chan acquired, 1)
	go func() {
		env, done, err := r.env.acquire()
		out <- acquired{env, done, err}
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

// ready brings up generation 0's environment for one create, which then
// registers a page and releases its hold, and returns the environment.
func (r *envRig) ready(pages int) *fakeEnv {
	r.t.Helper()
	pending := r.acquireAsync()
	gen := r.nextStart()
	e := &fakeEnv{id: int(gen) + 1}
	r.complete(gen, e)
	got := wait(r.t, pending)
	if got.err != nil || got.env != e {
		r.t.Fatalf("acquire = %v, %v; want the adopted environment", got.env, got.err)
	}
	r.setPages(pages)
	got.done()
	return e
}

func TestPaneEnvironmentReleasesWhenNoPageOrCreateRemains(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	e := r.ready(1)

	if released, reason := r.releaseIdle(); released {
		t.Fatal("released an environment with a page open")
	} else if reason != "1 page(s) open" {
		t.Fatalf("reason = %q", reason)
	}
	if freed := r.freedList(); len(freed) != 0 {
		t.Fatalf("freed %v with a page open", freed)
	}

	r.setPages(0)
	if released, reason := r.releaseIdle(); !released {
		t.Fatalf("kept an idle environment: %s", reason)
	}
	if freed := r.freedList(); len(freed) != 1 || freed[0] != e {
		t.Fatalf("freed %v, want the adopted environment once", freed)
	}
	if _, ok := r.env.current(); ok {
		t.Fatal("a released environment is still current")
	}
}

// A create holds the generation from acquire until its page is registered,
// whether it is still waiting on the cold create or already has the
// environment in hand.
func TestPaneEnvironmentKeepsAGenerationACreateHolds(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	pending := r.acquireAsync()
	gen := r.nextStart()

	if released, reason := r.releaseIdle(); released || reason != "1 create(s) in progress" {
		t.Fatalf("releaseIdle while a create waits = %v, %q", released, reason)
	}
	e := &fakeEnv{id: 1}
	r.complete(gen, e)
	got := wait(t, pending)
	if got.err != nil || got.env != e {
		t.Fatalf("the waiting create got %v, %v", got.env, got.err)
	}
	if released, _ := r.releaseIdle(); released {
		t.Fatal("released an environment a create holds before registering its page")
	}
	r.setPages(1)
	got.done()
	got.done() // idempotent
	if released, _ := r.releaseIdle(); released {
		t.Fatal("released the environment of the page the create registered")
	}
	if freed := r.freedList(); len(freed) != 0 {
		t.Fatalf("freed %v while in use", freed)
	}
}

func TestPaneEnvironmentRecreatesAfterARelease(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	first := r.ready(0)
	if released, _ := r.releaseIdle(); !released {
		t.Fatal("idle environment kept")
	}

	pending := r.acquireAsync()
	gen := r.nextStart()
	if gen != 1 {
		t.Fatalf("the create after a release started generation %d, want 1", gen)
	}
	second := &fakeEnv{id: 2}
	r.complete(gen, second)
	got := wait(t, pending)
	if got.err != nil || got.env != second {
		t.Fatalf("create after release got %v, %v; want the fresh environment", got.env, got.err)
	}
	got.done()
	if freed := r.freedList(); len(freed) != 1 || freed[0] != first {
		t.Fatalf("freed %v, want only the released environment", freed)
	}
}

// A create that gave up on a slow environment leaves the create in flight
// with no hold. A release then ends that generation, so the late completion
// frees its environment instead of adopting it, and the next create starts
// a fresh one.
func TestPaneEnvironmentReleaseRacingALateCompletion(t *testing.T) {
	r := newEnvRig(t, 30*time.Millisecond)
	pending := r.acquireAsync()
	gen := r.nextStart()
	if got := wait(t, pending); got.err == nil {
		t.Fatal("acquire did not time out")
	}
	if released, reason := r.releaseIdle(); !released {
		t.Fatalf("kept a generation no create holds: %s", reason)
	}

	late := &fakeEnv{id: 1}
	r.complete(gen, late)
	if freed := r.freedList(); len(freed) != 1 || freed[0] != late {
		t.Fatalf("freed %v, want the late environment", freed)
	}
	if _, ok := r.env.current(); ok {
		t.Fatal("a late completion of a released generation was adopted")
	}

	r.env.timeout = 5 * time.Second
	pending = r.acquireAsync()
	next := r.nextStart()
	if next == gen {
		t.Fatal("the next create reused the released generation")
	}
	fresh := &fakeEnv{id: 2}
	r.complete(next, fresh)
	if got := wait(t, pending); got.err != nil || got.env != fresh {
		t.Fatalf("next create got %v, %v", got.env, got.err)
	}
}

// Clear-data's release and an idle release can arrive in either order. Each
// environment is freed once, a create parked on the cleared generation is
// woken to retry at once, and the next create starts fresh.
func TestPaneEnvironmentReleaseRacingClearData(t *testing.T) {
	t.Run("idle then clear", func(t *testing.T) {
		r := newEnvRig(t, 5*time.Second)
		e := r.ready(0)
		if released, _ := r.releaseIdle(); !released {
			t.Fatal("idle environment kept")
		}
		r.do(r.env.release)
		if freed := r.freedList(); len(freed) != 1 || freed[0] != e {
			t.Fatalf("freed %v, want the environment once", freed)
		}
	})
	t.Run("clear wakes a parked create the idle release left alone", func(t *testing.T) {
		r := newEnvRig(t, 5*time.Second)
		pending := r.acquireAsync()
		gen := r.nextStart()
		if released, _ := r.releaseIdle(); released {
			t.Fatal("idle release took a generation a create waits on")
		}
		start := time.Now()
		r.do(r.env.release)
		got := wait(t, pending)
		if !errors.Is(got.err, errEnvironmentReleased) {
			t.Fatalf("parked create got %v, want the retry error", got.err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("the parked create sat out its timeout")
		}
		late := &fakeEnv{id: 1}
		r.complete(gen, late)
		if freed := r.freedList(); len(freed) != 1 || freed[0] != late {
			t.Fatalf("freed %v, want the cleared generation's late environment", freed)
		}
		if released, reason := r.releaseIdle(); released || reason != "no environment" {
			t.Fatalf("idle release after clear = %v, %q", released, reason)
		}
	})
	t.Run("clear then idle", func(t *testing.T) {
		r := newEnvRig(t, 5*time.Second)
		e := r.ready(0)
		r.do(r.env.release)
		if released, reason := r.releaseIdle(); released || reason != "no environment" {
			t.Fatalf("idle release after clear = %v, %q", released, reason)
		}
		if freed := r.freedList(); len(freed) != 1 || freed[0] != e {
			t.Fatalf("freed %v, want the environment once", freed)
		}
	})
}

func TestPaneEnvironmentRepeatedReleaseChangesNothing(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	r.ready(0)
	if released, _ := r.releaseIdle(); !released {
		t.Fatal("idle environment kept")
	}
	r.env.mu.Lock()
	gen := r.env.gen
	r.env.mu.Unlock()
	for range 3 {
		if released, reason := r.releaseIdle(); released || reason != "no environment" {
			t.Fatalf("repeated release = %v, %q", released, reason)
		}
	}
	r.env.mu.Lock()
	after := r.env.gen
	r.env.mu.Unlock()
	if after != gen {
		t.Fatalf("a repeated release moved the generation from %d to %d", gen, after)
	}
	if freed := r.freedList(); len(freed) != 1 {
		t.Fatalf("freed %d environments, want 1", len(freed))
	}
	r.noStart()
}

// A start queued for a generation a clear ended is skipped: the clear woke
// that generation's waiters and re-armed the start for the next create.
func TestPaneEnvironmentSkipsAStartForAReleasedGeneration(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	r.do(func() {
		r.env.mu.Lock()
		r.env.starting = true
		r.env.mu.Unlock()
		r.env.release()
		r.env.begin(0)
	})
	r.noStart()
}

// A failed create settles its generation for every waiter; an idle release
// then re-arms it so the next create tries again.
func TestPaneEnvironmentFailureSettlesAndIdleReleaseRearms(t *testing.T) {
	r := newEnvRig(t, 5*time.Second)
	pending := r.acquireAsync()
	gen := r.nextStart()
	r.do(func() { r.env.completed(gen, nil, errors.New("no runtime")) })
	if got := wait(t, pending); got.err == nil || got.err.Error() != "no runtime" {
		t.Fatalf("create got %v, want the settled failure", got.err)
	}
	if released, reason := r.releaseIdle(); !released {
		t.Fatalf("a failed generation was kept: %s", reason)
	}
	pending = r.acquireAsync()
	next := r.nextStart()
	if next == gen {
		t.Fatal("the next create did not start a new generation")
	}
	fresh := &fakeEnv{id: 2}
	r.complete(next, fresh)
	if got := wait(t, pending); got.err != nil || got.env != fresh {
		t.Fatalf("retry got %v, %v", got.env, got.err)
	}
}
