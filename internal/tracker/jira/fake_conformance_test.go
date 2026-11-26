package jira

import (
	"context"
	"encoding/json"
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

// The conformance test drives every exported Client method against the
// stateful fake Jira in internal/harness/trackerfake, so the fake and the
// client are held to the same contract.

const fakeSite = "example.atlassian.net"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeEnv struct {
	e     *trackerfake.Engine
	c     *Client
	clock *fakeClock
	ctx   context.Context
}

func fakeDoc(text string) json.RawMessage {
	return json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
}

// conformanceSite is the site every case starts from.
func conformanceSite(start time.Time) trackerfake.JiraSite {
	day := start.Add(-48 * time.Hour)
	return trackerfake.JiraSite{
		Site: fakeSite, Account: "acc-user1", Token: "conformance-token-1",
		Poker: &trackerfake.JiraPoker{},
		Users: []trackerfake.JiraUser{
			{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com"},
			{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com"},
		},
		Fields: []trackerfake.JiraField{
			{ID: "customfield_10016", Name: "Story Points", Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}},
			{ID: "customfield_10001", Name: "Area", Schema: trackerfake.JiraSchema{Type: "option", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:select", CustomID: 10001},
				Options: []trackerfake.JiraOption{{ID: "20001", Value: "Frontend"}, {ID: "20002", Value: "Backend"}}},
		},
		Projects: []trackerfake.JiraProject{
			{ID: "10000", Key: "WEB", Name: "Web", Components: []trackerfake.JiraNamed{{ID: "30001", Name: "UI"}}},
			{ID: "10001", Key: "APP", Name: "App", TypeKey: "service_desk"},
		},
		Boards: []trackerfake.JiraBoard{{ID: 1, Name: "WEB board", Type: "scrum", Project: "WEB", Estimation: "customfield_10016", Columns: []trackerfake.JiraColumn{
			{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}},
		}}},
		Sprints: []trackerfake.JiraSprint{
			{ID: 1, Board: 1, Name: "Sprint 1", State: "active", Start: day, End: day.Add(14 * 24 * time.Hour)},
			{ID: 2, Board: 1, Name: "Sprint 2", State: "future"},
		},
		Issues: []trackerfake.JiraIssue{
			{ID: "10101", Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "First story", Description: fakeDoc("first"), Sprints: []int64{1},
				Assignee: "acc-user1", Reporter: "acc-user1", Priority: "3", Created: day, Updated: day,
				Comments:    []trackerfake.JiraComment{{ID: "40001", Author: "acc-user2", Body: fakeDoc("a comment")}},
				Worklogs:    []trackerfake.JiraWorklog{{ID: "50001", Author: "acc-user1", Started: day, TimeSpent: 3600}},
				Attachments: []trackerfake.JiraAttachment{{ID: "60001", Filename: "notes.txt", MimeType: "text/plain", Content: []byte("hello")}},
				Watchers:    []string{"acc-user1"},
				Changelog: []trackerfake.JiraChange{{Author: "acc-user1", Created: day.Add(time.Hour), Items: []trackerfake.JiraChangeItem{
					{FieldID: "status", Field: "status", FieldType: "jira", From: ptr("10000"), To: ptr("10001"), FromString: ptr("To Do"), ToString: ptr("In Progress")}}}},
				Status: "10001"},
			{ID: "10102", Key: "WEB-2", Project: "WEB", Type: "10002", Summary: "Second task", Sprints: []int64{1}, Created: day, Updated: day},
			{ID: "10103", Key: "WEB-3", Project: "WEB", Type: "10000", Summary: "An epic", Created: day, Updated: day},
			{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10001", Summary: "Epic child", Parent: "10103", Created: day, Updated: day},
			{ID: "10201", Key: "APP-1", Project: "APP", Type: "10002", Summary: "A request", Created: day, Updated: day},
		},
	}
}

func ptr(s string) *string { return &s }

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{conformanceSite(clock.Now())}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, fakeSite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := New(Options{
		BaseURL: srv.JiraURL(fakeSite), Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte("conformance-token-1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request: %s", r.Detail)
		}
	})
	return &fakeEnv{e: e, c: c, clock: clock, ctx: context.Background()}
}

// result is a call's value and error, for got(call()).must(t).
type result[T any] struct {
	v   T
	err error
}

func got[T any](v T, err error) result[T] { return result[T]{v, err} }

func (r result[T]) must(t *testing.T) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// issueOf is an issue of the fake's current state.
func (f *fakeEnv) issueOf(t *testing.T, key string) trackerfake.JiraIssue {
	t.Helper()
	for _, is := range f.e.Snapshot().Jira[0].Issues {
		if is.Key == key {
			return is
		}
	}
	t.Fatalf("no issue %s", key)
	return trackerfake.JiraIssue{}
}

// conformanceCases drive each exported Client method; a case checks the
// call succeeded and what it read or wrote.
var conformanceCases = func() map[string]func(t *testing.T, f *fakeEnv) {
	out := maps.Clone(issueCases)
	maps.Copy(out, boardCases)
	maps.Copy(out, pokerCases)
	maps.Copy(out, devCases)
	return out
}()

// TestFakeConformance runs every case against a fresh fake.
func TestFakeConformance(t *testing.T) {
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range conformanceCases {
			if !yield(k) {
				return
			}
		}
	}) {
		t.Run(name, func(t *testing.T) { conformanceCases[name](t, newFakeEnv(t)) })
	}
}

// TestFakeConformanceCoversClient fails for an exported Client method
// with no conformance case, so a new method needs one.
func TestFakeConformanceCoversClient(t *testing.T) {
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; conformanceCases[name] == nil {
			t.Errorf("Client.%s has no conformance case against the fake", name)
		}
	}
	for name := range conformanceCases {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("conformance case %s names no Client method", name)
		}
	}
}
