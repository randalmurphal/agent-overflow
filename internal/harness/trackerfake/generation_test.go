package trackerfake

import (
	"context"
	"testing"
	"time"
)

// gate is a world whose held request stops in OnRequest, so a test can
// change the engine between its hold ending and its answer.
type gate struct {
	*world
	entered chan struct{}
	resume  chan struct{}
}

func newGate(t *testing.T) *gate {
	t.Helper()
	g := &gate{world: &world{clock: &testClock{now: testStart}, seen: make(chan Request, 64)},
		entered: make(chan struct{}), resume: make(chan struct{})}
	g.e = New(Options{Now: g.clock.Now, OnRequest: func(r Request) {
		if r.Held {
			close(g.entered)
			<-g.resume
		}
	}})
	t.Cleanup(g.e.Close)
	if err := g.e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(g.e, testSite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	g.srv = srv
	g.arm(t, Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold})
	return g
}

// TestHeldRequestOutlivesItsWorld lets a held comment go and then, before
// it runs, changes the world; a request from a world that is gone is
// dropped with nothing applied, and one from the same world is answered.
func TestHeldRequestOutlivesItsWorld(t *testing.T) {
	for _, c := range []struct {
		name    string
		change  func(t *testing.T, e *Engine)
		dropped bool
	}{
		{"release then nothing", func(t *testing.T, e *Engine) { release(t, e) }, false},
		{"clear then nothing", func(t *testing.T, e *Engine) { e.ClearFaults(jiraOrigin, commentRoute) }, false},
		{"release then reset and reseed", func(t *testing.T, e *Engine) { release(t, e); e.Reset(); reseed(t, e) }, true},
		{"clear then reset and reseed", func(t *testing.T, e *Engine) { e.ClearFaults(jiraOrigin, commentRoute); e.Reset(); reseed(t, e) }, true},
		{"release then reseed the site", func(t *testing.T, e *Engine) { release(t, e); reseed(t, e) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGate(t)
			answered := g.comment(context.Background())
			select {
			case <-g.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the comment was never held")
			}
			c.change(t, g.e)
			close(g.resume)
			got := <-answered
			comments := g.comments(t)
			switch {
			case c.dropped && (got.err == nil || comments != 0):
				t.Fatalf("answered %+v with %d comments; want the connection closed and none", got, comments)
			case !c.dropped && (got.err != nil || got.r.status != 201 || comments != 1):
				t.Fatalf("answered %+v with %d comments; want 201 and one", got, comments)
			}
		})
	}
}

func release(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Release(jiraOrigin, commentRoute, false); err != nil {
		t.Fatal(err)
	}
}

func reseed(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Seed(testFixture()); err != nil {
		t.Fatal(err)
	}
}

// TestSeedOfAnotherTenantKeepsTheWorld: adding a tenant is not a new
// world, so a held request is still answered.
func TestSeedOfAnotherTenantKeepsTheWorld(t *testing.T) {
	g := newGate(t)
	answered := g.comment(context.Background())
	<-g.entered
	release(t, g.e)
	other := testFixture()
	other.Jira[0].Site, other.Jira[0].Token, other.Jira[0].Tempo = "other.atlassian.net", "jira-token-0002", nil
	other.Linear = nil
	if err := g.e.Seed(other); err != nil {
		t.Fatal(err)
	}
	close(g.resume)
	if got := <-answered; got.err != nil || got.r.status != 201 {
		t.Fatalf("answered %+v", got)
	}
}
