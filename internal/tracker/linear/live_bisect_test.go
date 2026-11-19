//go:build linearlive

package linear

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveHistoryFoldBisect narrows the creation fold TestLiveHistoryFold
// bracketed to 75s..4m, for one field and across fields. Six issues each
// get one priority change, at +90s, +120s, ..., +240s after their own
// creation, so the fold's end for priority is bracketed to 30s. Three
// more issues get an assignee change at +5s, a state change at +225s and,
// when the team uses estimates, an estimate change at +225s, so a field
// whose fold differs from priority's shows. Histories are read 30s after
// the last change (six times the 5s an entry was seen to lag) and again
// 30s later. It runs about five minutes and makes under 50 requests:
//
//	LINEAR_LIVE_KEY_FILE=/path/to/key go test -tags linearlive -run '^TestLiveHistoryFoldBisect$' -v -count=1 -timeout 15m ./internal/tracker/linear
func TestLiveHistoryFoldBisect(t *testing.T) {
	l := newLive(t)
	var viewer *Viewer
	l.must("Viewer", func(ctx context.Context) (err error) { viewer, err = l.c.Viewer(ctx); return err })
	var teams []Team
	l.must("Teams", func(ctx context.Context) (err error) { teams, err = l.c.Teams(ctx); return err })
	if len(teams) == 0 {
		t.Fatal("the workspace has no team")
	}
	team := teams[0]
	var states []WorkflowState
	l.must("WorkflowStates", func(ctx context.Context) (err error) { states, err = l.c.WorkflowStates(ctx, team.ID); return err })
	l.sweep(team)

	var plan []*bisectChange
	for i := range 6 {
		at := 90*time.Second + time.Duration(i)*30*time.Second
		plan = append(plan, &bisectChange{field: "priority", at: at, in: UpdateIssueInput{Priority: Set(PriorityHigh)},
			carries: func(e HistoryEntry) bool { return e.Priority != nil }})
	}
	plan = append(plan, &bisectChange{field: "assignee", at: 5 * time.Second, in: UpdateIssueInput{AssigneeID: Set(viewer.User.ID)},
		carries: func(e HistoryEntry) bool { return e.Assignee != nil }})
	plan = append(plan, &bisectChange{field: "state", at: 225 * time.Second,
		carries: func(e HistoryEntry) bool { return e.State != nil }})
	if team.Estimation.Scale != EstimateNotUsed && team.Estimation.Scale != "" {
		plan = append(plan, &bisectChange{field: "estimate", at: 225 * time.Second, in: UpdateIssueInput{Estimate: Set(2)},
			carries: func(e HistoryEntry) bool { return e.Estimate != nil }})
	} else {
		t.Logf("no estimate change: team %s does not use estimates", team.Key)
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i, c := range plan {
		l.must(fmt.Sprintf("CreateIssue bisect %d", i), func(ctx context.Context) (err error) {
			c.issue, err = l.c.CreateIssue(ctx, CreateIssueInput{TeamID: team.ID, Title: fmt.Sprintf("%s%s bisect %d %s", liveTitle, stamp, i, c.field),
				Description: "Test data from the agent-overflow live check. Safe to delete."})
			return err
		})
		c.born = time.Now()
		issue := c.issue
		t.Cleanup(func() { l.cleanup(issue) })
		if c.field == "state" {
			s := slices.IndexFunc(states, func(s WorkflowState) bool { return s.Type == "started" && s.ID != issue.StateID })
			if s < 0 {
				t.Fatalf("team %s has no started state to move %s to", team.Key, issue.Identifier)
			}
			c.in = UpdateIssueInput{StateID: Set(states[s].ID)}
		}
	}

	byTime := slices.Clone(plan)
	slices.SortStableFunc(byTime, func(a, b *bisectChange) int { return a.born.Add(a.at).Compare(b.born.Add(b.at)) })
	for _, c := range byTime {
		if d := time.Until(c.born.Add(c.at)); d > 0 {
			<-time.After(d)
		}
		if l.step(fmt.Sprintf("UpdateIssue %s %s", c.issue.Identifier, c.field), func(ctx context.Context) error {
			_, err := l.c.UpdateIssue(ctx, c.issue.ID, c.in)
			return err
		}) {
			c.made = time.Since(c.born)
			t.Logf("%s %s change made at +%s", c.issue.Identifier, c.field, c.made.Round(time.Millisecond))
		}
	}
	last := byTime[len(byTime)-1]
	for read := 1; read <= 2; read++ {
		if d := time.Until(last.born.Add(last.at).Add(time.Duration(read) * 30 * time.Second)); d > 0 {
			<-time.After(d)
		}
		bisectRead(l, plan, read)
	}
	bisectReport(l, plan)
}

// bisectChange is one issue and its one planned change.
type bisectChange struct {
	field   string
	at      time.Duration // after the issue's creation
	in      UpdateIssueInput
	carries func(HistoryEntry) bool
	issue   *Issue
	born    time.Time     // when the create returned
	made    time.Duration // after born; zero when the update failed
	// recorded is, per read, whether the history held an entry carrying
	// the change.
	recorded [3]bool
}

// bisectRead reads every issue's history in one request and marks each
// change recorded or not.
func bisectRead(l *live, plan []*bisectChange, read int) {
	ids := make([]string, 0, len(plan))
	for _, c := range plan {
		ids = append(ids, c.issue.ID)
	}
	l.step(fmt.Sprintf("IssueHistories read %d", read), func(ctx context.Context) error {
		got, err := l.c.IssueHistories(ctx, ids)
		if err != nil {
			return err
		}
		if len(got) != len(plan) {
			return fmt.Errorf("%d histories for %d issues", len(got), len(plan))
		}
		for _, h := range got {
			i := slices.IndexFunc(plan, func(c *bisectChange) bool { return c.issue.ID == h.IssueID })
			for _, e := range h.Entries {
				l.t.Logf("read %d %s %s", read, plan[i].issue.Identifier, entryString(e))
			}
			plan[i].recorded[read] = slices.ContainsFunc(h.Entries, plan[i].carries)
		}
		return nil
	})
}

// bisectReport logs each change's outcome, then the priority bracket and
// whether any other field disagrees with it.
func bisectReport(l *live, plan []*bisectChange) {
	var priority []*bisectChange
	lateSecond := false
	for _, c := range plan {
		if c.made == 0 {
			l.measure("%s change on %s: the update failed, so it shows nothing", c.field, c.issue.Identifier)
			continue
		}
		l.measure("%s change at +%s on %s: recorded in read 1 %t, read 2 %t", c.field, c.made.Round(time.Second),
			c.issue.Identifier, c.recorded[1], c.recorded[2])
		lateSecond = lateSecond || (c.recorded[2] && !c.recorded[1])
		if c.field == "priority" {
			priority = append(priority, c)
		}
	}
	slices.SortFunc(priority, func(a, b *bisectChange) int { return cmp.Compare(a.made, b.made) })
	var lines []string
	if lateSecond {
		lines = append(lines, "an entry appeared only in the second read: lateness exceeds 30s, so read 2 decides")
	}
	// The bracket: folded up to the latest unrecorded priority change,
	// recorded from the earliest recorded one; a recorded change before an
	// unrecorded one breaks it.
	lower, upper, monotone := time.Duration(0), time.Duration(-1), true
	for _, c := range priority {
		switch {
		case !c.recorded[2] && upper >= 0:
			monotone = false
		case !c.recorded[2]:
			lower = c.made
		case upper < 0:
			upper = c.made
		}
	}
	switch {
	case len(priority) == 0:
		lines = append(lines, "priority: no change was made, no bracket")
	case !monotone:
		lines = append(lines, "priority: no window, a later priority change was folded after an earlier one was recorded")
	case upper < 0:
		lines = append(lines, fmt.Sprintf("priority: every change through +%s was folded; the fold ends after it", lower.Round(time.Second)))
	case lower == 0:
		lines = append(lines, fmt.Sprintf("priority: every change from +%s was recorded; the fold ends at or before it", upper.Round(time.Second)))
	default:
		lines = append(lines, fmt.Sprintf("priority: the fold ends after +%s and at or before +%s", lower.Round(time.Second), upper.Round(time.Second)))
	}
	for _, c := range plan {
		if c.field == "priority" || c.made == 0 || !monotone || len(priority) == 0 {
			continue
		}
		var predicted string
		switch {
		case c.made <= lower:
			predicted = "folded"
		case upper >= 0 && c.made >= upper:
			predicted = "recorded"
		default:
			lines = append(lines, fmt.Sprintf("%s at +%s: inside priority's bracket, says nothing (%s)", c.field,
				c.made.Round(time.Second), map[bool]string{true: "recorded", false: "folded"}[c.recorded[2]]))
			continue
		}
		got := map[bool]string{true: "recorded", false: "folded"}[c.recorded[2]]
		verdict := "agrees"
		if got != predicted {
			verdict = "disagrees"
		}
		lines = append(lines, fmt.Sprintf("%s at +%s: %s, priority's bracket predicts %s: %s", c.field, c.made.Round(time.Second), got, predicted, verdict))
	}
	l.measure("CONCLUSION\n  %s", strings.Join(lines, "\n  "))
}
