//go:build linearlive

package linear

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveHistoryFold measures when Linear writes an issue's history
// entries, to tell three hypotheses apart:
//
//   - late: entries are written some time after the change, so a read
//     right after it misses one a later read finds;
//   - window: changes within a fixed time of creation are folded into the
//     creation and never become entries, later ones always do;
//   - merge: an entry absorbs later edits made close to it, so one entry
//     carries several changes or changes between reads.
//
// Issue x is changed at about +2s, +75s, +4m and +11m after creation and
// read at once, 20s and 60s after each change. Issue y is left alone
// until about +12m, then edited three times 5s apart (title, priority,
// title again) and read after each edit and 20s and 60s after the last.
// One final read, by both the batched and the single-issue call, ends
// the run. It runs about 14 minutes, makes under 50 requests, and is not
// part of TestLive:
//
//	LINEAR_LIVE_KEY_FILE=/path/to/key go test -tags linearlive -run '^TestLiveHistoryFold$' -v -count=1 -timeout 30m ./internal/tracker/linear
func TestLiveHistoryFold(t *testing.T) {
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

	stamp := time.Now().UTC().Format("20060102T150405Z")
	f := &foldRun{l: l, last: map[string]string{}}
	for _, name := range []string{"x", "y"} {
		var issue *Issue
		l.must("CreateIssue fold "+name, func(ctx context.Context) (err error) {
			issue, err = l.c.CreateIssue(ctx, CreateIssueInput{TeamID: team.ID, Title: liveTitle + stamp + " fold " + name,
				Description: "Test data from the agent-overflow live check. Safe to delete."})
			return err
		})
		f.issues = append(f.issues, issue)
		f.born = append(f.born, time.Now())
		t.Cleanup(func() { l.cleanup(issue) })
		t.Logf("created %s %s createdAt %s", name, issue.Identifier, issue.CreatedAt.Format(time.RFC3339Nano))
	}
	x, y := f.issues[0], f.issues[1]
	i := slices.IndexFunc(states, func(s WorkflowState) bool { return s.Type == "started" && s.ID != x.StateID })
	if i < 0 {
		t.Fatalf("team %s has no started state to move x to", team.Key)
	}
	started := states[i].ID
	priority := func(p Priority) func(HistoryEntry) bool {
		return func(e HistoryEntry) bool {
			return e.Priority != nil && e.Priority.To != nil && *e.Priority.To == float64(p)
		}
	}
	title := func(s string) func(HistoryEntry) bool {
		return func(e HistoryEntry) bool { return e.Title != nil && e.Title.To == s }
	}
	f.changes = []*foldChange{
		{issue: 0, label: "x priority Low", at: 2 * time.Second, in: UpdateIssueInput{Priority: Set(PriorityLow)}, match: priority(PriorityLow)},
		{issue: 0, label: "x state started", at: 75 * time.Second, in: UpdateIssueInput{StateID: Set(started)},
			match: func(e HistoryEntry) bool { return e.State != nil && e.State.To == started }},
		{issue: 0, label: "x assignee viewer", at: 4 * time.Minute, in: UpdateIssueInput{AssigneeID: Set(viewer.User.ID)},
			match: func(e HistoryEntry) bool { return e.Assignee != nil && e.Assignee.To == viewer.User.ID }},
		{issue: 0, label: "x priority High", at: 11 * time.Minute, in: UpdateIssueInput{Priority: Set(PriorityHigh)}, match: priority(PriorityHigh)},
		{issue: 1, label: "y title edit 1", at: 12*time.Minute + 5*time.Second, in: UpdateIssueInput{Title: Set(y.Title + " edit 1")}, match: title(y.Title + " edit 1")},
		{issue: 1, label: "y priority Medium", at: 12*time.Minute + 10*time.Second, in: UpdateIssueInput{Priority: Set(PriorityMedium)}, match: priority(PriorityMedium)},
		{issue: 1, label: "y title edit 3", at: 12*time.Minute + 15*time.Second, in: UpdateIssueInput{Title: Set(y.Title + " edit 3")}, match: title(y.Title + " edit 3")},
	}
	f.run()
	f.report(x, y)
}

// foldChange is one planned change and what the reads showed of it.
type foldChange struct {
	issue int // index into foldRun.issues
	label string
	at    time.Duration // after the issue's creation
	in    UpdateIssueInput
	match func(HistoryEntry) bool
	made  time.Time // zero when the update failed
	// next is the read made right after the change; seen is when a read
	// first found an entry matching the change, and read which read it
	// was; entries are every entry ID that matched.
	next    int
	seen    time.Time
	read    int
	entries []string
}

// foldRun is the run's issues, changes and what each read found.
type foldRun struct {
	l       *live
	issues  []*Issue
	born    []time.Time // when each create returned
	changes []*foldChange
	reads   int
	// last is each entry's entryString at the latest read that held it,
	// and latest each issue's entry IDs at the latest read.
	last   map[string]string
	latest map[string][]string
	// rewrites and vanished are entries that changed or disappeared
	// between reads.
	rewrites, vanished []string
}

// run makes each change at its time, reading at once, and reads 20s and
// 60s after every x change and after the last y change, then once more.
func (f *foldRun) run() {
	type event struct {
		at time.Time
		do func()
	}
	var events []event
	for i, c := range f.changes {
		at := f.born[c.issue].Add(c.at)
		events = append(events, event{at, func() { f.change(c); f.read("after " + c.label) }})
		if c.issue == 0 || i == len(f.changes)-1 {
			for _, d := range []time.Duration{20 * time.Second, time.Minute} {
				events = append(events, event{at.Add(d), func() { f.read(fmt.Sprintf("%s after %s", d, c.label)) }})
			}
		}
	}
	slices.SortStableFunc(events, func(a, b event) int { return a.at.Compare(b.at) })
	for _, e := range events {
		if d := time.Until(e.at); d > 0 {
			<-time.After(d)
		}
		e.do()
	}
	if d := time.Until(events[len(events)-1].at.Add(5 * time.Second)); d > 0 {
		<-time.After(d)
	}
	f.read("final")
}

func (f *foldRun) change(c *foldChange) {
	issue := f.issues[c.issue]
	if f.l.step("UpdateIssue "+c.label, func(ctx context.Context) error {
		_, err := f.l.c.UpdateIssue(ctx, issue.ID, c.in)
		return err
	}) {
		c.made, c.next = time.Now(), f.reads+1
		f.l.t.Logf("%s made %s after the create returned", c.label, c.made.Sub(f.born[c.issue]).Round(time.Millisecond))
	}
}

// read reads both histories in one request, logs every entry, notes
// entries that changed or disappeared since the last read, and marks the
// changes the entries now show.
func (f *foldRun) read(label string) {
	f.reads++
	n := f.reads
	label = fmt.Sprintf("IssueHistories read %d at +%s (%s)", n, time.Since(f.born[0]).Round(time.Second), label)
	f.l.step(label, func(ctx context.Context) error {
		got, err := f.l.c.IssueHistories(ctx, []string{f.issues[0].ID, f.issues[1].ID})
		if err != nil {
			return err
		}
		now := time.Now()
		present := map[string]bool{}
		f.latest = map[string][]string{}
		for _, h := range got {
			for _, e := range h.Entries {
				f.latest[h.IssueID] = append(f.latest[h.IssueID], e.ID)
				s := entryString(e)
				present[e.ID] = true
				f.l.t.Logf("read %d %s %s", n, f.name(h.IssueID), s)
				if before, ok := f.last[e.ID]; ok && before != s {
					f.rewrites = append(f.rewrites, fmt.Sprintf("read %d: %s was %q", n, s, before))
				}
				f.last[e.ID] = s
				for _, c := range f.changes {
					if c.made.IsZero() || f.issues[c.issue].ID != h.IssueID || !c.match(e) {
						continue
					}
					if c.seen.IsZero() {
						c.seen, c.read = now, n
					}
					if !slices.Contains(c.entries, e.ID) {
						c.entries = append(c.entries, e.ID)
					}
				}
			}
		}
		for id := range f.last {
			if !present[id] && !strings.HasPrefix(f.last[id], "gone ") {
				f.vanished = append(f.vanished, fmt.Sprintf("read %d: %s", n, f.last[id]))
				f.last[id] = "gone " + f.last[id]
			}
		}
		return nil
	})
}

func (f *foldRun) name(issueID string) string {
	if issueID == f.issues[0].ID {
		return "x"
	}
	return "y"
}

// report logs one MEASURE line per observation and the conclusion the
// observations support.
func (f *foldRun) report(x, y *Issue) {
	l := f.l
	for _, c := range f.changes {
		offset := c.made.Sub(f.born[c.issue]).Round(time.Second)
		switch {
		case c.made.IsZero():
			l.measure("%s: the update failed, so it shows nothing", c.label)
		case c.seen.IsZero():
			l.measure("%s (made at +%s): no entry in any read through the final one, %s after it", c.label, offset,
				time.Since(c.made).Round(time.Second))
		default:
			l.measure("%s (made at +%s): first seen %s after it, at read %d, in entries %v", c.label, offset,
				c.seen.Sub(c.made).Round(time.Second), c.read, c.entries)
		}
	}
	for _, r := range f.rewrites {
		l.measure("an entry changed between reads: %s", r)
	}
	for _, v := range f.vanished {
		l.measure("an entry disappeared between reads: %s", v)
	}
	for _, issue := range []*Issue{x, y} {
		l.step("IssueHistory single-issue cross-check "+f.name(issue.ID), func(ctx context.Context) error {
			p, err := l.c.IssueHistory(ctx, issue.ID, "")
			if err != nil {
				return err
			}
			var single []string
			for _, e := range p.Items {
				single = append(single, e.ID)
			}
			batched := f.latest[issue.ID]
			l.measure("the single-issue history of %s agrees with the final batched read: %t (single %v, batched %v)",
				f.name(issue.ID), slices.Equal(single, batched), single, batched)
			return nil
		})
	}
	f.conclude()
}

// conclude states which hypotheses the observations rule out.
func (f *foldRun) conclude() {
	l := f.l
	var made, seen []*foldChange
	late := false
	for _, c := range f.changes {
		if c.made.IsZero() {
			continue
		}
		made = append(made, c)
		if !c.seen.IsZero() {
			seen = append(seen, c)
			// A first sighting after the read right after the change is a
			// late write.
			if c.read > c.next {
				late = true
			}
		}
	}
	lines := []string{fmt.Sprintf("%d of %d changes became entries", len(seen), len(made))}
	switch {
	case len(seen) == 0:
		lines = append(lines, "late: not testable, no change ever became an entry")
	case late:
		lines = append(lines, "late: supported, an entry first appeared in a read after the one right after its change")
	default:
		lines = append(lines, "late: ruled out for every change that became an entry; each was in the read right after it")
	}
	// Window: x's changes, in time order, must be missing up to some
	// point and present after it; y's late edits must then be present.
	var xs []*foldChange
	for _, c := range made {
		if c.issue == 0 {
			xs = append(xs, c)
		}
	}
	monotone, lower, upper := true, time.Duration(0), time.Duration(-1)
	for _, c := range xs {
		offset := c.made.Sub(f.born[0])
		if c.seen.IsZero() {
			if upper >= 0 {
				monotone = false
			}
			lower = offset
		} else if upper < 0 {
			upper = offset
		}
	}
	ySeen := slices.ContainsFunc(made, func(c *foldChange) bool { return c.issue == 1 && !c.seen.IsZero() })
	switch {
	case !monotone:
		lines = append(lines, "window: ruled out, a later change of x became an entry while an earlier one did not, or the reverse")
	case upper < 0:
		lines = append(lines, fmt.Sprintf("window: if it holds, the fold is longer than %s (no change of x became an entry)", lower.Round(time.Second)))
	case !ySeen:
		lines = append(lines, fmt.Sprintf("window: ruled out as the whole rule, x is folded only up to %s yet none of y's edits at about +12m became entries",
			upper.Round(time.Second)))
	default:
		lines = append(lines, fmt.Sprintf("window: consistent, the fold is longer than %s and at most %s", lower.Round(time.Second), upper.Round(time.Second)))
	}
	merged := slices.ContainsFunc(f.changes, func(c *foldChange) bool {
		return slices.ContainsFunc(f.changes, func(d *foldChange) bool {
			return c != d && slices.ContainsFunc(c.entries, func(id string) bool { return slices.Contains(d.entries, id) })
		})
	})
	var ys []*foldChange
	for _, c := range made {
		if c.issue == 1 {
			ys = append(ys, c)
		}
	}
	distinct := len(ys) == 3 && !slices.ContainsFunc(ys, func(c *foldChange) bool { return len(c.entries) != 1 })
	switch {
	case merged || len(f.rewrites) > 0 || len(f.vanished) > 0:
		lines = append(lines, fmt.Sprintf("merge: supported, entries shared changes (%t), changed between reads (%d) or disappeared (%d)",
			merged, len(f.rewrites), len(f.vanished)))
	case distinct:
		lines = append(lines, "merge: ruled out for edits 5s apart, y's three edits are three entries none of which changed")
	default:
		lines = append(lines, "merge: not settled, y's edits did not each become one entry, and no entry was shared or changed")
	}
	l.measure("CONCLUSION\n  %s", strings.Join(lines, "\n  "))
}
