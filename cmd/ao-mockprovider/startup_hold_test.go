package main

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// startupRig is a mock whose scenario holds its start behind "held-start"
// and whose onStart waits on a step gate (named or not), driven through
// its real adapter's stdin handler with the engine goroutine running, as
// main runs them. Every goroutine it starts ends before the test does.
type startupRig struct {
	t       *testing.T
	e       *engine
	out     *syncBuffer
	reports chan control.Report
	// announce feeds the adapter the stdin line that makes it write its
	// first frame; announced is closed once that handler returned.
	announce  func()
	announced chan struct{}
	// ran is closed when the engine goroutine returns.
	ran                           chan struct{}
	engineStarted, adapterStarted bool
	// first is a fragment of the frame that proves the provider is up.
	first string
}

var startupProviders = []string{scenario.ProviderClaude, scenario.ProviderCodex, scenario.ProviderAgy}

func newStartupRig(t *testing.T, provider, stepGate string) *startupRig {
	t.Helper()
	out := &syncBuffer{}
	sc := &scenario.Scenario{
		Version:     scenario.CurrentVersion,
		Name:        "slow-start",
		Provider:    provider,
		StartupGate: "held-start",
		OnStart:     []scenario.Step{{WaitSignal: &scenario.WaitSignalStep{Name: stepGate}}},
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	reports := make(chan control.Report, 64)
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(out), &reporter{observe: func(r control.Report) { reports <- r }},
		scenario.Vars{"SESSION_ID": "s1", "THREAD_ID": "th-1", "CWD": "/ws"})
	e.exitFn = func(code int) { t.Errorf("unexpected exit(%d)", code) }
	r := &startupRig{t: t, e: e, out: out, reports: reports, announced: make(chan struct{}), ran: make(chan struct{})}
	switch provider {
	case scenario.ProviderClaude:
		a := newClaudeAdapter(e, newLineWriter(out), nil)
		e.adapter = a
		r.announce = func() { a.handleLine([]byte(userLine)) }
		r.first = `"subtype":"init"`
	case scenario.ProviderCodex:
		a := newCodexAdapter(e, newLineWriter(out), nil)
		e.adapter = a
		r.announce = func() { a.handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)) }
		r.first = `"id":1`
	case scenario.ProviderAgy:
		a := newACPAdapter(e, newLineWriter(out))
		e.adapter = a
		r.announce = func() {
			a.handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`))
		}
		r.first = `"protocolVersion"`
	}
	t.Cleanup(r.stop)
	return r
}

func (r *startupRig) startEngine() {
	r.engineStarted = true
	go func() {
		r.e.run()
		close(r.ran)
	}()
}

func (r *startupRig) startAdapter() {
	r.adapterStarted = true
	go func() {
		r.announce()
		close(r.announced)
	}()
}

// stop releases whatever a failed test left held, ends the engine
// goroutine, and fails the test when a goroutine the rig started is still
// running.
func (r *startupRig) stop() {
	r.e.hold.mu.Lock()
	held := !r.e.hold.done
	r.e.hold.mu.Unlock()
	if held {
		r.e.releaseStartup()
	}
	r.e.advance("")
	if r.adapterStarted {
		r.ended("the adapter's stdin handler", r.announced)
	}
	close(r.e.turnCh)
	if r.engineStarted {
		r.ended("the engine goroutine", r.ran)
	}
}

func (r *startupRig) ended(what string, done chan struct{}) {
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.t.Errorf("%s was still running when the test ended", what)
	}
}

// await waits for the report that match accepts, failing on one that
// refuse accepts first.
func (r *startupRig) await(what string, match, refuse func(control.Report) bool) {
	r.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case rep := <-r.reports:
			if refuse != nil && refuse(rep) {
				r.t.Fatalf("got %+v before %s", rep, what)
			}
			if match(rep) {
				return
			}
		case <-deadline:
			r.t.Fatalf("no %s within 5s", what)
		}
	}
}

func (r *startupRig) waitingOn(gate string, startup bool) {
	r.t.Helper()
	r.await("waiting_signal for "+gate, func(rep control.Report) bool {
		return rep.Kind == control.ReportWaitingSignal && rep.Detail == gate && rep.Startup == startup
	}, nil)
}

// released waits for the advance_released report of gate, failing on a
// release of any other gate first.
func (r *startupRig) released(gate string, startup bool) {
	r.t.Helper()
	r.await("release of "+gate, func(rep control.Report) bool {
		return rep.Kind == control.ReportAdvanceReleased && rep.Gate == gate && rep.Startup == startup
	}, func(rep control.Report) bool {
		return rep.Kind == control.ReportAdvanceReleased && (rep.Gate != gate || rep.Startup != startup)
	})
}

func (r *startupRig) expectAnnounced() {
	r.t.Helper()
	select {
	case <-r.announced:
	case <-time.After(5 * time.Second):
		r.t.Fatal("the provider never announced itself after its start was advanced")
	}
	if !strings.Contains(r.out.String(), r.first) {
		r.t.Fatalf("the first frame %s was not written:\n%s", r.first, r.out.String())
	}
}

func (r *startupRig) expectHeld() {
	r.t.Helper()
	select {
	case <-r.announced:
		r.t.Fatal("the provider announced itself before its start was advanced")
	default:
	}
	if strings.Contains(r.out.String(), r.first) {
		r.t.Fatalf("the first frame was written before its start was advanced:\n%s", r.out.String())
	}
}

// expectStepGateHeld fails when the onStart step gate has been released.
func (r *startupRig) expectStepGateHeld() {
	r.t.Helper()
	r.e.mu.Lock()
	open := r.e.gate != nil
	r.e.mu.Unlock()
	if !open {
		r.t.Fatal("the onStart step gate was released by an advance naming the startup gate")
	}
}

// The startup gate and an onStart step gate are held together, in either
// opening order, and each is released by the advance naming it.
func TestStartupGateAndOnStartGateHoldTogether(t *testing.T) {
	for _, provider := range startupProviders {
		t.Run(provider+"/onStart first", func(t *testing.T) {
			r := newStartupRig(t, provider, "boot")
			r.startEngine()
			r.waitingOn("boot", false)
			r.startAdapter()
			r.waitingOn("held-start", true)
			r.expectHeld()
			r.e.advance("held-start")
			r.released("held-start", true)
			r.expectAnnounced()
			r.e.advance("boot")
			r.released("boot", false)
		})
		t.Run(provider+"/start first", func(t *testing.T) {
			r := newStartupRig(t, provider, "boot")
			r.startAdapter()
			r.waitingOn("held-start", true)
			r.startEngine()
			r.waitingOn("boot", false)
			r.e.advance("boot")
			r.released("boot", false)
			r.expectHeld()
			r.e.advance("held-start")
			r.released("held-start", true)
			r.expectAnnounced()
		})
	}
}

// An advance naming the startup gate that arrives before the adapter
// reaches the hold, before the first turn has begun, releases the hold
// when it is reached: the advance is the process's, not turn 0's.
func TestAStartupAdvanceBeforeTheHoldReleasesIt(t *testing.T) {
	for _, provider := range startupProviders {
		t.Run(provider, func(t *testing.T) {
			r := newStartupRig(t, provider, "boot")
			r.e.advance("held-start")
			r.released("held-start", true)
			r.startAdapter()
			r.expectAnnounced()
			r.startEngine()
			r.waitingOn("boot", false)
			r.e.advance("boot")
			r.released("boot", false)
		})
	}
}

// With an unnamed onStart gate open, which any other advance releases, an
// advance naming the startup gate is still the startup gate's: the step
// gate stays held, and the hold is released when the adapter reaches it.
func TestAStartupAdvanceIsNotTakenByAnUnnamedStepGate(t *testing.T) {
	for _, provider := range startupProviders {
		t.Run(provider, func(t *testing.T) {
			r := newStartupRig(t, provider, "")
			r.startEngine()
			r.waitingOn("", false)
			r.e.advance("held-start")
			r.released("held-start", true)
			r.expectStepGateHeld()
			r.startAdapter()
			r.expectAnnounced()
			// A second advance with the startup name has nothing to release
			// and is discarded rather than passed on to the step gate.
			r.e.advance("held-start")
			r.await("the discard of a second startup advance", func(rep control.Report) bool {
				return rep.Kind == control.ReportAdvanceBuffered && rep.Startup && rep.Detail == control.AdvanceDroppedDetail
			}, func(rep control.Report) bool { return rep.Kind == control.ReportAdvanceReleased })
			r.expectStepGateHeld()
			r.e.advance("")
			r.released("", false)
		})
	}
}

// The hold is paid once; a per-frame hold would turn every later turn into
// a held start.
func TestStartupGateHoldsTheFirstFrameOnce(t *testing.T) {
	r := newStartupRig(t, scenario.ProviderClaude, "boot")
	r.e.advance("held-start")
	for i := range 2 {
		passed := make(chan struct{})
		go func() {
			r.e.awaitStartup()
			close(passed)
		}()
		select {
		case <-passed:
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d was held after the startup gate was released", i+1)
		}
	}
}
