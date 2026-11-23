package linear

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// The conformance test drives every exported Client method, and so every
// registered operation, against the stateful fake Linear in
// internal/harness/trackerfake.

type conformanceClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *conformanceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conformanceClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fid is a fixture ID in Linear's lowercase UUID form.
func fid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

var (
	cfUser1, cfUser2, cfUser3              = fid(1), fid(2), fid(3)
	cfTeam                                 = fid(10)
	cfBacklog, cfTodo, cfStarted           = fid(20), fid(21), fid(22)
	cfDone, cfCanceled                     = fid(23), fid(24)
	cfPast, cfActive, cfUpcoming           = fid(30), fid(31), fid(32)
	cfBug, cfFeature                       = fid(40), fid(41)
	cfProject                              = fid(50)
	cfIssue1, cfIssue2, cfIssue3           = fid(60), fid(61), fid(62)
	cfComment, cfAttachment, cfRel         = fid(70), fid(71), fid(72)
	cfStart                                = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	cfDay                                  = 24 * time.Hour
	cfThree                        float64 = 3
)

func conformanceWorkspace() trackerfake.LinearWorkspace {
	at := func(days int) time.Time { return cfStart.Add(time.Duration(days) * cfDay) }
	sub := 1.0
	return trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_conformance1", Viewer: cfUser1,
		Users: []trackerfake.LinearUser{
			{ID: cfUser1, Name: "User 1", Email: "user1@example.com", Admin: true},
			{ID: cfUser2, Name: "User 2", Email: "user2@example.com"},
			{ID: cfUser3, Name: "User 3", Email: "user3@example.com", Disabled: true},
		},
		Teams: []trackerfake.LinearTeam{{
			ID: cfTeam, Key: "WEB", Name: "Web", Timezone: "UTC", Estimation: "fibonacci",
			CyclesEnabled: true, CycleWeeks: 2, DefaultState: cfTodo, Members: []string{cfUser1, cfUser2},
		}},
		States: []trackerfake.LinearState{
			{ID: cfBacklog, Team: cfTeam, Name: "Backlog", Type: "backlog", Position: 0},
			{ID: cfTodo, Team: cfTeam, Name: "Todo", Type: "unstarted", Position: 1},
			{ID: cfStarted, Team: cfTeam, Name: "In Progress", Type: "started", Position: 2},
			{ID: cfDone, Team: cfTeam, Name: "Done", Type: "completed", Position: 3},
			{ID: cfCanceled, Team: cfTeam, Name: "Canceled", Type: "canceled", Position: 4},
		},
		Cycles: []trackerfake.LinearCycle{
			{ID: cfPast, Team: cfTeam, Number: 1, StartsAt: at(-24), EndsAt: at(-10), CompletedAt: at(-10)},
			{ID: cfActive, Team: cfTeam, Number: 2, StartsAt: at(-10), EndsAt: at(4)},
			{ID: cfUpcoming, Team: cfTeam, Number: 3, StartsAt: at(4), EndsAt: at(18)},
		},
		Labels: []trackerfake.LinearLabel{
			{ID: cfBug, Name: "Bug", Team: cfTeam},
			{ID: cfFeature, Name: "Feature"},
		},
		Projects: []trackerfake.LinearProject{{ID: cfProject, Name: "Website", Teams: []string{cfTeam}, StatusName: "In Progress", StatusType: "started"}},
		Issues: []trackerfake.LinearIssue{
			{
				ID: cfIssue1, Team: cfTeam, Number: 1, Title: "Fix login", State: cfTodo, Assignee: cfUser1, Creator: cfUser1,
				Cycle: cfActive, Project: cfProject, Labels: []string{cfBug}, Priority: 2, Estimate: &cfThree, SortOrder: 100,
				CreatedAt: at(-18), UpdatedAt: at(-1),
				History: []trackerfake.LinearHistory{{CreatedAt: at(-9), Actor: cfUser1, FromState: cfBacklog, ToState: cfTodo}},
			},
			{ID: cfIssue2, Team: cfTeam, Number: 2, Title: "Add search", State: cfBacklog, Creator: cfUser2, SortOrder: 200, CreatedAt: at(-17), UpdatedAt: at(-2)},
			{
				ID: cfIssue3, Team: cfTeam, Number: 3, Title: "Login tests", State: cfStarted, Parent: cfIssue1, Creator: cfUser1,
				SortOrder: 300, SubIssueSortOrder: &sub, CreatedAt: at(-16), UpdatedAt: at(-3), StartedAt: at(-3),
			},
		},
		Comments:    []trackerfake.LinearComment{{ID: cfComment, Issue: cfIssue1, User: cfUser2, Body: "Looks good", CreatedAt: at(-2)}},
		Attachments: []trackerfake.LinearAttachment{{ID: cfAttachment, Issue: cfIssue1, Title: "Pull request 1", URL: "https://example.com/pr/1", Creator: cfUser1}},
		Relations:   []trackerfake.LinearRelation{{ID: cfRel, Type: "blocks", Issue: cfIssue1, Related: cfIssue2}},
	}
}

type conformanceEnv struct {
	e     *trackerfake.Engine
	c     *Client
	clock *conformanceClock
	ctx   context.Context
}

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()
	return newFakeEnv(t, conformanceWorkspace())
}

// newFakeEnv is a client of a stateful fake holding w, whose key is the
// conformance workspace's.
func newFakeEnv(t *testing.T, w trackerfake.LinearWorkspace) *conformanceEnv {
	t.Helper()
	clock := &conformanceClock{now: cfStart}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{w}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := New(Options{
		BaseURL: srv.LinearURL(), Version: "test", Workspace: "example",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte("lin_api_conformance1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s: %s", r.Operation, r.Detail)
		}
	})
	return &conformanceEnv{e: e, c: c, clock: clock, ctx: context.Background()}
}

func (f *conformanceEnv) state() trackerfake.LinearWorkspace { return f.e.Snapshot().Linear[0] }

func (f *conformanceEnv) issue(t *testing.T, id string) trackerfake.LinearIssue {
	t.Helper()
	for _, is := range f.state().Issues {
		if is.ID == id {
			return is
		}
	}
	t.Fatalf("issue %s is not in the fake", id)
	return trackerfake.LinearIssue{}
}

type callAnswer[T any] struct {
	v   T
	err error
}

func answered[T any](v T, err error) callAnswer[T] { return callAnswer[T]{v, err} }

func (r callAnswer[T]) must(t *testing.T) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

func identifiers(is []Issue) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Identifier)
	}
	return out
}

func want(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func TestFakeConformance(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]bool{}
	all := true
	for _, name := range slices.Sorted(maps.Keys(linearCases)) {
		all = t.Run(name, func(t *testing.T) {
			f := newConformanceEnv(t)
			linearCases[name](t, f)
			mu.Lock()
			defer mu.Unlock()
			for _, r := range f.e.Requests(0).Requests {
				sent[r.Operation] = true
			}
		}) && all
	}
	if !all {
		return
	}
	// Every operation the package registers must have run against the
	// fake, so that a new operation needs a case.
	for _, name := range slices.Sorted(maps.Keys(registeredOps())) {
		if !sent[name] {
			t.Errorf("operation %s never ran against the fake", name)
		}
	}
}

// TestFakeConformanceCoversClient fails for an exported Client method
// with no conformance case.
func TestFakeConformanceCoversClient(t *testing.T) {
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; linearCases[name] == nil {
			t.Errorf("Client.%s has no conformance case against the fake", name)
		}
	}
	for name := range linearCases {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("conformance case %s names no Client method", name)
		}
	}
}
