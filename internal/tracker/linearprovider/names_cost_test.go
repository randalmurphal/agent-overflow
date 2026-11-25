package linearprovider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

var (
	namesApp       = sid(11)
	namesAppState  = sid(25)
	namesOld       = sid(51)
	namesGoneUser  = sid(91)
	namesGoneCycle = sid(92)
	namesGoneProj  = sid(93)
	namesGoneTeam  = sid(94)
	namesGoneIssue = sid(95)
	namesGoneLabel = sid(96)
)

const (
	namesEveryKind = 11 // issues 10 to 20 name every kind
	namesLong      = 30 // its history runs past three pages
	namesGone      = 31 // names entities the workspace lacks
	namesLongN     = 100 + 250 + 1
)

// namesWorkspace holds tickets whose histories name a user, a cycle, a
// project, a team, a parent and labels: issue 1 an assignee and a cycle
// change, issues 10 to 20 every kind, issue 30 an assignee change in each
// of namesLongN entries, and issue 31 every kind by IDs the workspace
// lacks, as a deleted or invisible entity is.
func namesWorkspace(now time.Time) trackerfake.LinearWorkspace {
	w := costWorkspace(now)
	w.Issues = w.Issues[:40]
	w.Comments = nil
	for i := range w.Issues {
		w.Issues[i].History = nil
	}
	w.Teams = append(w.Teams, trackerfake.LinearTeam{ID: namesApp, Key: "APP", Name: "App", DefaultState: namesAppState})
	w.States = append(w.States, trackerfake.LinearState{ID: namesAppState, Team: namesApp, Name: "Todo", Type: "unstarted"})
	w.Projects = append(w.Projects, trackerfake.LinearProject{ID: namesOld, Name: "Old", Teams: []string{costTeam}})
	at := func(is *trackerfake.LinearIssue, n int) time.Time {
		return is.CreatedAt.Add(time.Hour + time.Duration(n)*time.Minute)
	}
	one := &w.Issues[1]
	one.History = []trackerfake.LinearHistory{{ID: sid(200001), CreatedAt: at(one, 0), Actor: costUsers[0],
		ToAssignee: costUsers[1], FromCycle: costActive, ToCycle: costNext}}
	for i := 10; i < 10+namesEveryKind; i++ {
		is := &w.Issues[i]
		is.History = []trackerfake.LinearHistory{{ID: sid(210000 + i), CreatedAt: at(is, 0), Actor: costUsers[0],
			FromAssignee: costUsers[0], ToAssignee: costUsers[1], FromCycle: costActive, ToCycle: costNext,
			FromProject: namesOld, ToProject: costProject, FromTeam: namesApp, ToTeam: costTeam, ToParent: costIssueID(0),
			AddedLabels: []string{costLabels[0]}, RemovedLabels: []string{costLabels[1]}}}
	}
	long := &w.Issues[namesLong]
	for e := range namesLongN {
		long.History = append(long.History, trackerfake.LinearHistory{ID: sid(300000 + e), CreatedAt: at(long, e), Actor: costUsers[0],
			FromAssignee: costUsers[e%3], ToAssignee: costUsers[(e+1)%3]})
	}
	gone := &w.Issues[namesGone]
	gone.History = []trackerfake.LinearHistory{{ID: sid(400000), CreatedAt: at(gone, 0), Actor: costUsers[0],
		FromAssignee: namesGoneUser, ToAssignee: costUsers[1], FromCycle: namesGoneCycle, FromProject: namesGoneProj,
		FromTeam: namesGoneTeam, ToTeam: costTeam, FromParent: namesGoneIssue, AddedLabels: []string{namesGoneLabel, costLabels[0]}}}
	return w
}

// eventLine is d's history as Field:From>To, oldest first.
func eventLine(d tracker.Detail) string {
	var out []string
	for _, e := range d.History {
		out = append(out, e.Field+":"+e.From+">"+e.To)
	}
	return strings.Join(out, " ")
}

// A ticket read carries the names its history shows: a Details of
// tickets whose histories name every kind is one request per
// linear.MaxIssueDetails issues, a history past its first page one more
// request a further page, and none of them reads a name by ID. An entity
// Linear answers null for (deleted, or not visible) is shown by its ID.
// Requests are counted as the fake receives them.
func TestDetailNamesCost(t *testing.T) {
	r, _ := newCostRunOf(t, namesWorkspace)
	p, ctx := r.p, t.Context()
	ids := func(from, n int) []tracker.IssueID { return costIDs(from, n) }
	every := "Assignee:User 1>User 2 Cycle:Cycle 30>Cycle 31 Project:Old>Launch Parent:>WEB-1 Team:APP App>WEB Web Labels:Feature>Bug"
	details := func(label string, issues []tracker.IssueID, check func(d tracker.Detail) error) {
		r.measure(label, func() error {
			got, err := p.Details(ctx, issues)
			if err != nil {
				return err
			}
			if len(got.Details) != len(issues) || len(got.Missing) != 0 {
				return fmt.Errorf("%d details, missing %v", len(got.Details), got.Missing)
			}
			for _, d := range got.Details {
				if err := check(d); err != nil {
					return fmt.Errorf("%s: %w", d.Card.Key, err)
				}
			}
			return nil
		})
	}
	events := func(want string) func(tracker.Detail) error {
		return func(d tracker.Detail) error {
			if got := eventLine(d); got != want {
				return fmt.Errorf("events\n got %s\nwant %s", got, want)
			}
			return nil
		}
	}

	r.measure("Identity (first call)", func() error { _, err := p.Identity(ctx); return err })
	details("Details (1, assignee and cycle)", ids(1, 1), events("Assignee:>User 2 Cycle:Cycle 30>Cycle 31"))
	details("Details (10, every kind)", ids(10, linear.MaxIssueDetails), events(every))
	details("Details (11, every kind)", ids(10, namesEveryKind), events(every))
	details("Details (351 entries)", ids(namesLong, 1), func(d tracker.Detail) error {
		if len(d.History) != namesLongN {
			return fmt.Errorf("%d events", len(d.History))
		}
		for _, e := range d.History {
			if !strings.HasPrefix(e.From, "User ") || !strings.HasPrefix(e.To, "User ") {
				return fmt.Errorf("event %+v is not named", e)
			}
		}
		return nil
	})
	details("Details (gone entities)", ids(namesGone, 1), events(fmt.Sprintf("Assignee:%s>User 2 Cycle:%s> Project:%s> Parent:%s> Team:%s>WEB Web Labels:>%s, Bug",
		namesGoneUser, namesGoneCycle, namesGoneProj, namesGoneIssue, namesGoneTeam, namesGoneLabel)))

	r.pin(map[string]pinned{
		"Identity (first call)":           {2, 6},
		"Details (1, assignee and cycle)": {1, 2679},
		"Details (10, every kind)":        {1, 2679},
		"Details (11, every kind)":        {2, 5358},
		"Details (351 entries)":           {3, 12809},
		"Details (gone entities)":         {1, 2679},
	})
}
