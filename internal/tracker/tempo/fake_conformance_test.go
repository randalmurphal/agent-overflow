package tempo

import (
	"context"
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
// stateful fake Tempo in internal/harness/trackerfake.

const fakeSite = "example.atlassian.net"

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// staticIssues resolves the fixture's issues.
type staticIssues map[string]int64

func (f staticIssues) IDs(_ context.Context, keys []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, k := range keys {
		if id, ok := f[k]; ok {
			out[k] = id
		}
	}
	return out, nil
}

type fakeWorld struct {
	e   *trackerfake.Engine
	c   *Client
	ctx context.Context
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	clock := &stepClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	site := trackerfake.JiraSite{
		Site: fakeSite, Account: "acc-user1", Token: "conformance-jira-1",
		Users:    []trackerfake.JiraUser{{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com"}},
		Projects: []trackerfake.JiraProject{{ID: "10000", Key: "WEB", Name: "Web"}},
		Issues: []trackerfake.JiraIssue{
			{ID: "10101", Key: "WEB-1", Project: "WEB", Summary: "First"},
			{ID: "10102", Key: "WEB-2", Project: "WEB", Summary: "Second"},
		},
		Tempo: &trackerfake.TempoAccount{
			Token: "conformance-tempo-1",
			Attributes: []trackerfake.TempoAttribute{{Key: "_Kind_", Name: "Kind", Type: "STATIC_LIST", Values: []string{"dev", "ops"},
				Names: map[string]string{"dev": "Development", "ops": "Operations"}}},
			Accounts:  []trackerfake.TempoBilling{{ID: 1, Key: "ACME", Name: "Acme", Status: "OPEN"}, {ID: 2, Key: "OLD", Name: "Old", Status: "CLOSED"}},
			Holidays:  []trackerfake.TempoHoliday{{Date: "2026-11-20", Name: "Day off"}},
			Approvals: []trackerfake.TempoApproval{{Account: "acc-user1", From: "2026-11-09", To: "2026-11-15", Status: "APPROVED", Reviewer: "acc-user1", Actor: "acc-user1"}},
			Worklogs: []trackerfake.TempoWorklog{{ID: 7001, Issue: "10101", Author: "acc-user1", StartDate: "2026-11-18", StartTime: "09:00:00",
				Spent: 3600, Description: "Work", Attributes: []trackerfake.TempoAttrPair{{Key: "_Kind_", Value: "dev"}}}},
		},
	}
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, fakeSite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := New(Options{
		BaseURL: srv.TempoURL(), Version: "test", Issues: staticIssues{"WEB-1": 10101, "WEB-2": 10102},
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte("conformance-tempo-1")))),
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
	return &fakeWorld{e: e, c: c, ctx: context.Background()}
}

type callResult[T any] struct {
	v   T
	err error
}

func called[T any](v T, err error) callResult[T] { return callResult[T]{v, err} }

func (r callResult[T]) must(t *testing.T) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

func (f *fakeWorld) worklogs() []trackerfake.TempoWorklog {
	return f.e.Snapshot().Jira[0].Tempo.Worklogs
}

func dateIn(t *testing.T, s string) Date { return called(ParseDate(s)).must(t) }

func newDraft(t *testing.T, date string) Draft {
	return Draft{
		Issue: Issue{Key: "WEB-2"}, Author: "acc-user1", Start: Start{Date: dateIn(t, date), Time: called(Clock(10, 0, 0)).must(t)},
		Spent: 30 * time.Minute, Description: "New work", Attributes: []AttributeValue{{Key: "_Kind_", Value: "ops"}},
	}
}

var conformanceCases = map[string]func(t *testing.T, f *fakeWorld){
	"Attributes": func(t *testing.T, f *fakeWorld) {
		if as := called(f.c.Attributes(f.ctx)).must(t); len(as) != 1 || len(as[0].Options) != 2 {
			t.Fatalf("attributes %+v", as)
		}
	},
	"Settings": func(t *testing.T, f *fakeWorld) {
		if s := called(f.c.Settings(f.ctx)).must(t); s.ApprovalPeriod != "WEEKLY" || s.WeekStart != time.Monday {
			t.Fatalf("settings %+v", s)
		}
	},
	"DropCache":        func(t *testing.T, f *fakeWorld) { f.c.DropCache(); called(f.c.Settings(f.ctx)).must(t) },
	"ForgetCredential": func(t *testing.T, f *fakeWorld) { f.c.ForgetCredential(); called(f.c.Settings(f.ctx)).must(t) },
	"Close": func(t *testing.T, f *fakeWorld) {
		f.c.Close()
		if _, err := f.c.Settings(f.ctx); err == nil {
			t.Fatal("a closed client answered")
		}
	},
	"Accounts": func(t *testing.T, f *fakeWorld) {
		if as := called(f.c.Accounts(f.ctx, AccountQuery{Statuses: []AccountStatus{AccountOpen}})).must(t); len(as) != 1 || as[0].Key != "ACME" {
			t.Fatalf("accounts %+v", as)
		}
	},
	"Schedule": func(t *testing.T, f *fakeWorld) {
		days := called(f.c.Schedule(f.ctx, "acc-user1", dateIn(t, "2026-11-19"), dateIn(t, "2026-11-21"))).must(t)
		if len(days) != 3 || days[0].Kind != WorkingDay || days[1].Kind != HolidayDay || days[2].Kind != NonWorkingDay {
			t.Fatalf("schedule %+v", days)
		}
	},
	"Approval": func(t *testing.T, f *fakeWorld) {
		if a := called(f.c.Approval(f.ctx, "acc-user1", dateIn(t, "2026-11-10"))).must(t); a.Status != ApprovalApproved {
			t.Fatalf("approval %+v", a)
		}
	},
	"Worklog": func(t *testing.T, f *fakeWorld) {
		if w := called(f.c.Worklog(f.ctx, 7001)).must(t); w.Issue != (Issue{ID: 10101}) || w.Spent != time.Hour {
			t.Fatalf("worklog %+v", w)
		}
	},
	"Worklogs": func(t *testing.T, f *fakeWorld) {
		ws := called(f.c.Worklogs(f.ctx, WorklogQuery{Author: "acc-user1", From: dateIn(t, "2026-11-16"), To: dateIn(t, "2026-11-22")})).must(t)
		if len(ws) != 1 {
			t.Fatalf("worklogs %+v", ws)
		}
	},
	"WorklogPages": func(t *testing.T, f *fakeWorld) {
		n := 0
		err := f.c.WorklogPages(f.ctx, WorklogQuery{Author: "acc-user1", From: dateIn(t, "2026-11-18"), To: dateIn(t, "2026-11-18")}, func(ws []Worklog) error {
			n += len(ws)
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("pages: %d worklogs, %v", n, err)
		}
	},
	"Create": func(t *testing.T, f *fakeWorld) {
		w := called(f.c.Create(f.ctx, newDraft(t, "2026-11-19"))).must(t)
		if w.ID == 0 || len(f.worklogs()) != 2 {
			t.Fatalf("created %+v", w)
		}
	},
	"Update": func(t *testing.T, f *fakeWorld) {
		from := called(f.c.Worklog(f.ctx, 7001)).must(t)
		spent := 2 * time.Hour
		called(f.c.Update(f.ctx, Change{From: from, Spent: &spent})).must(t)
		if f.worklogs()[0].Spent != 7200 {
			t.Fatal("not updated")
		}
	},
	"Delete": func(t *testing.T, f *fakeWorld) {
		if err := f.c.Delete(f.ctx, 7001); err != nil {
			t.Fatal(err)
		}
		if len(f.worklogs()) != 0 {
			t.Fatal("not deleted")
		}
	},
	"Validate": func(t *testing.T, f *fakeWorld) {
		reports := called(f.c.Validate(f.ctx, []Op{CreateOp(newDraft(t, "2026-11-19")), CreateOp(newDraft(t, "2026-11-10"))})).must(t)
		if len(reports) != 2 || len(reports[0].Problems) != 0 || len(reports[1].Problems) == 0 {
			t.Fatalf("reports %+v", reports)
		}
	},
	"Apply": func(t *testing.T, f *fakeWorld) {
		out := f.c.Apply(f.ctx, []Op{CreateOp(newDraft(t, "2026-11-19")), DeleteOp(7001)})
		if len(out) != 2 || out[0].Status != Applied || out[1].Status != Applied {
			t.Fatalf("outcomes %+v", out)
		}
		if ws := f.worklogs(); len(ws) != 1 || ws[0].Issue != "10102" {
			t.Fatalf("worklogs %+v", ws)
		}
	},
}

func TestFakeConformance(t *testing.T) {
	names := make([]string, 0, len(conformanceCases))
	for name := range conformanceCases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) { conformanceCases[name](t, newFakeWorld(t)) })
	}
}

// TestFakeConformanceCoversClient fails for an exported Client method
// with no conformance case.
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
