package trackerfake

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// outcome is how an in-process request ended.
type outcome struct {
	code   int
	closed bool
	// panicked is a panic other than the abort that closes a connection.
	panicked string
}

// startComment sends a comment to g's Jira handler in the background,
// with body as the request body.
func (g *rig) startComment(body io.Reader) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		req := httptest.NewRequest("POST", "http://"+testSite+"/rest/api/3/issue/WEB-1/comment", body)
		req.Header.Set("Authorization", basic(testJiraToken)["Authorization"])
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		defer func() {
			switch p := recover(); {
			case p == http.ErrAbortHandler:
				out <- outcome{closed: true}
			case p != nil:
				out <- outcome{panicked: fmt.Sprint(p)}
			default:
				out <- outcome{code: rec.Code}
			}
		}()
		g.h[jiraOrigin].ServeHTTP(rec, req)
	}()
	return out
}

const commentBody = `{"body":` + testDoc + `}`

// stalledBody is a request body whose first read reports itself and
// waits for go, so a test can act while the request is entering the
// pipeline, before it takes the engine's lock.
type stalledBody struct {
	started, goAhead chan struct{}
	once             sync.Once
	r                io.Reader
}

func newStalledBody() *stalledBody {
	return &stalledBody{started: make(chan struct{}), goAhead: make(chan struct{}), r: strings.NewReader(commentBody)}
}

func (b *stalledBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.goAhead
	})
	return b.r.Read(p)
}

func within(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

func await(t *testing.T, c <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-c:
		if o.panicked != "" {
			t.Fatalf("the request panicked: %s", o.panicked)
		}
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("the request was never answered")
	}
	return outcome{}
}

// TestEveryWorldEndMeetsEveryRequestState ends a world (Reset, a Seed
// that replaces the site, Close) while a comment is in each state: still
// entering (and then held in the new world), held, released but not yet
// resumed, and answered. A request still entering at the end is answered
// in the new world; one held across it, released or not, is dropped
// with nothing applied; one already answered stays answered.
func TestEveryWorldEndMeetsEveryRequestState(t *testing.T) {
	transitions := []struct {
		name string
		end  func(t *testing.T, e *Engine)
		// open: the engine still answers, with the site seeded again.
		open bool
	}{
		{"reset", func(t *testing.T, e *Engine) { e.Reset(); reseed(t, e) }, true},
		{"replacing seed", func(t *testing.T, e *Engine) { reseed(t, e) }, true},
		{"close", func(t *testing.T, e *Engine) { e.Close() }, false},
	}
	for _, tr := range transitions {
		t.Run(tr.name+"/entering", func(t *testing.T) {
			g := newRig(t, testFixture())
			body := newStalledBody()
			answered := g.startComment(body)
			within(t, body.started, "the body read")
			tr.end(t, g.e)
			close(body.goAhead)
			got := await(t, answered)
			switch n := commentsIn(g.e); {
			case tr.open && (got.closed || got.code != 201 || n != 1):
				t.Fatalf("ended %+v with %d comments; want 201 in the new world", got, n)
			case !tr.open && (!got.closed || n != 0):
				t.Fatalf("ended %+v with %d comments; want the connection closed", got, n)
			}
		})
		t.Run(tr.name+"/entering, then held", func(t *testing.T) {
			g, holding, _ := heldRig(t, false)
			body := newStalledBody()
			answered := g.startComment(body)
			within(t, body.started, "the body read")
			tr.end(t, g.e)
			if tr.name == "reset" {
				if err := g.e.AddFault(Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold}); err != nil {
					t.Fatal(err)
				}
			}
			close(body.goAhead)
			if tr.open {
				within(t, holding, "the hold")
				release(t, g.e)
			}
			got := await(t, answered)
			switch n := commentsIn(g.e); {
			case tr.open && (got.closed || got.code != 201 || n != 1):
				t.Fatalf("ended %+v with %d comments; want 201 in the new world", got, n)
			case !tr.open && (!got.closed || n != 0):
				t.Fatalf("ended %+v with %d comments; want the connection closed", got, n)
			}
		})
		t.Run(tr.name+"/held", func(t *testing.T) {
			g, holding, _ := heldRig(t, false)
			answered := g.startComment(strings.NewReader(commentBody))
			within(t, holding, "the hold")
			tr.end(t, g.e)
			dropped(t, g.e, await(t, answered))
		})
		t.Run(tr.name+"/released, not resumed", func(t *testing.T) {
			g, holding, resume := heldRig(t, true)
			answered := g.startComment(strings.NewReader(commentBody))
			within(t, holding, "the hold")
			release(t, g.e)
			tr.end(t, g.e)
			close(resume)
			dropped(t, g.e, await(t, answered))
		})
		t.Run(tr.name+"/resumed", func(t *testing.T) {
			g, holding, _ := heldRig(t, false)
			answered := g.startComment(strings.NewReader(commentBody))
			within(t, holding, "the hold")
			release(t, g.e)
			if got := await(t, answered); got.closed || got.code != 201 || commentsIn(g.e) != 1 {
				t.Fatalf("released: %+v", got)
			}
			tr.end(t, g.e)
			if n, want := commentsIn(g.e), map[bool]int{true: 0, false: 1}[tr.open]; n != want {
				t.Fatalf("after the end: %d comments, want %d", n, want)
			}
		})
	}
}

// heldRig is a rig whose comment route is held. holding closes when a
// comment is held; with stall, the held request then waits in
// OnRequest until resume closes, before it waits on its hold.
func heldRig(t *testing.T, stall bool) (g *rig, holding, resume chan struct{}) {
	t.Helper()
	g = newRig(t, testFixture())
	holding, resume = make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.e.opts.OnRequest = func(r Request) {
		if !r.Held {
			return
		}
		once.Do(func() { close(holding) })
		if stall {
			<-resume
		}
	}
	if err := g.e.AddFault(Fault{Origin: jiraOrigin, Route: commentRoute, Kind: FaultHold}); err != nil {
		t.Fatal(err)
	}
	return g, holding, resume
}

func dropped(t *testing.T, e *Engine, got outcome) {
	t.Helper()
	if n := commentsIn(e); !got.closed || n != 0 {
		t.Fatalf("ended %+v with %d comments; want the connection closed and none", got, n)
	}
}

func commentsIn(e *Engine) int {
	return len(e.Snapshot().Jira[0].Issues[0].Comments)
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

// TestReplacingSeedKeepsArmedHolds: a hold still armed when a Seed
// replaces the site holds the next request until it is released.
func TestReplacingSeedKeepsArmedHolds(t *testing.T) {
	g, holding, _ := heldRig(t, false)
	reseed(t, g.e)
	answered := g.startComment(strings.NewReader(commentBody))
	within(t, holding, "the hold")
	select {
	case got := <-answered:
		t.Fatalf("answered before its release: %+v", got)
	default:
	}
	release(t, g.e)
	if got := await(t, answered); got.closed || got.code != 201 {
		t.Fatalf("released: %+v", got)
	}
}

// TestSeedOfAnotherTenantKeepsTheWorld: adding a tenant is not a new
// world, so a held request released before it is still answered.
func TestSeedOfAnotherTenantKeepsTheWorld(t *testing.T) {
	g, holding, resume := heldRig(t, true)
	answered := g.startComment(strings.NewReader(commentBody))
	within(t, holding, "the hold")
	release(t, g.e)
	other := testFixture()
	other.Jira[0].Site, other.Jira[0].Token, other.Jira[0].Tempo = "other.atlassian.net", "jira-token-0002", nil
	other.Linear = nil
	if err := g.e.Seed(other); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if got := await(t, answered); got.closed || got.code != 201 {
		t.Fatalf("answered %+v", got)
	}
}
