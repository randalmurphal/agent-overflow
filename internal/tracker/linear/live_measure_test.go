//go:build linearlive

package linear

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Bounds of the measuring steps' waits.
const (
	// foldWait is how long after creation the late change is made.
	foldWait = 70 * time.Second
	// searchWait bounds the wait for a new title to be searchable, polled
	// every searchPoll.
	searchWait = 60 * time.Second
	searchPoll = 5 * time.Second
)

// measure logs one answer to an open question, one line each.
func (l *live) measure(format string, args ...any) {
	l.t.Helper()
	l.t.Logf("MEASURE: "+format, args...)
}

// pair is the two issues the measuring steps create: a holds the search
// word and the immediate change, b the due date edits.
type pair struct {
	a, b    *Issue
	bornAt  time.Time // when a's create returned
	word    string
	trashed bool // b was trashed by a step; its cleanup is not needed
}

// checkAdditions exercises the calls added for the provider (ID and
// identifier filters, IssueSummary, SearchIssues, IssueHistories, due
// dates) and measures what the provider assumes, each answer a MEASURE
// line.
func (l *live) checkAdditions(team Team, viewer *Viewer, states []WorkflowState, stamp string) {
	since := time.Now().Add(-time.Minute)
	p := l.createPair(team, stamp)
	if p == nil {
		return
	}
	l.checkDueDate(p.b)
	l.checkIDFilters(team, p)
	l.checkSearch(team, p)
	if wait := time.Until(p.bornAt.Add(foldWait)); wait > 0 {
		l.t.Logf("waiting %s so the late change is %s after creation", wait.Round(time.Second), foldWait)
		<-time.After(wait)
	}
	l.checkHistories(team, viewer, states, p)
	l.checkEqualOrder(team, p)
	l.checkArchivedAndTrashed(team, since, p)
}

// createPair creates the two issues, a with a due date and a word no other
// title holds, and changes a's priority at once, for the fold measure.
func (l *live) createPair(team Team, stamp string) *pair {
	p := &pair{word: "qz" + strings.Map(func(r rune) rune {
		if '0' <= r && r <= '9' {
			return 'a' + (r - '0')
		}
		return -1
	}, stamp)}
	for i, title := range []string{"history a " + p.word, "history b"} {
		var issue *Issue
		if !l.step(fmt.Sprintf("CreateIssue pair %d", i), func(ctx context.Context) (err error) {
			in := CreateIssueInput{TeamID: team.ID, Title: liveTitle + stamp + " " + title,
				Description: "Test data from the agent-overflow live check. Safe to delete."}
			if i == 1 {
				in.DueDate = Set(Date("2026-12-01"))
			}
			issue, err = l.c.CreateIssue(ctx, in)
			if err == nil && i == 1 && issue.DueDate != "2026-12-01" {
				err = fmt.Errorf("created with due date %q", issue.DueDate)
			}
			return err
		}) {
			return nil
		}
		if i == 0 {
			p.a, p.bornAt = issue, time.Now()
			l.t.Cleanup(func() { l.cleanup(p.a) })
			l.step("UpdateIssue at once after create", func(ctx context.Context) error {
				_, err := l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{Priority: Set(PriorityLow)})
				return err
			})
			l.t.Logf("immediate change made %s after the create returned", time.Since(p.bornAt).Round(time.Millisecond))
		} else {
			p.b = issue
			l.t.Cleanup(func() {
				if !p.trashed {
					l.cleanup(p.b)
				}
			})
		}
	}
	return p
}

// checkDueDate reads a due date set on create, changed, and cleared back.
func (l *live) checkDueDate(issue *Issue) {
	readBack := func(name string, want Date) {
		l.step("IssueSummary due date "+name, func(ctx context.Context) error {
			got, err := l.c.IssueSummary(ctx, issue.ID)
			if err != nil {
				return err
			}
			if name == "cleared" {
				l.measure("after dueDate: null the due date reads back as %q (empty is cleared)", got.DueDate)
			}
			if got.DueDate != want {
				return fmt.Errorf("due date %q, want %q", got.DueDate, want)
			}
			return nil
		})
	}
	readBack("created", "2026-12-01")
	l.step("UpdateIssue due date", func(ctx context.Context) error {
		_, err := l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{DueDate: Set(Date("2027-01-31"))})
		return err
	})
	readBack("changed", "2027-01-31")
	l.step("UpdateIssue due date null", func(ctx context.Context) error {
		_, err := l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{DueDate: Null[Date]()})
		return err
	})
	readBack("cleared", "")
}

// idsOf is the IDs of issues, in order.
func idsOf(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.ID)
	}
	return out
}

// sameSet reports whether got and want hold the same IDs.
func sameSet(got, want []string) bool {
	return len(got) == len(want) && !slices.ContainsFunc(want, func(id string) bool { return !slices.Contains(got, id) })
}

// checkIDFilters reads the pair by an ID set and by identifiers, each with
// one that names nothing, and each alone with IssueSummary; it measures
// a page's charged cost against the documented rule, Linear's answer to
// a non-UUID in an ID filter, and its own case folding of a team key.
func (l *live) checkIDFilters(team Team, p *pair) {
	want := []string{p.a.ID, p.b.ID}
	l.step("ListIssues by IDs", func(ctx context.Context) error {
		pg, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{IDs: []string{p.a.ID, p.b.ID, "00000000-0000-4000-8000-000000000000"}}, Limit: 100}, "")
		if err != nil {
			return err
		}
		rule, rerr := ruleCost(opIssues.text, map[string]int{"first": 100})
		l.measure("an issue page with first: 100 holding %d issues was charged %d points; the documented rule gives %.1f (rule error %v)",
			len(pg.Items), l.c.Usage().Complexity, rule, rerr)
		if !sameSet(idsOf(pg.Items), want) {
			return fmt.Errorf("the ID filter answered %v, want %v", idsOf(pg.Items), want)
		}
		return nil
	})
	l.step("ListIssues by identifiers", func(ctx context.Context) error {
		keys := []string{p.a.Identifier, strings.ToLower(p.b.Identifier), team.Key + "-999999"}
		pg, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Identifiers: keys}}, "")
		if err == nil && !sameSet(idsOf(pg.Items), want) {
			err = fmt.Errorf("the identifier filter %v answered %v, want %v", keys, idsOf(pg.Items), want)
		}
		return err
	})
	for _, key := range []string{p.a.ID, p.b.Identifier} {
		l.step("IssueSummary "+key, func(ctx context.Context) error {
			got, err := l.c.IssueSummary(ctx, key)
			if err == nil && got.ID != p.a.ID && got.ID != p.b.ID {
				err = fmt.Errorf("%s answered issue %s", key, got.ID)
			}
			return err
		})
	}
	sort, _ := OrderManual.sort()
	raw := func(name string, filter issueFilterInput) (Page[Issue], error) {
		var pg Page[Issue]
		err := errors.New("not run")
		l.step(name, func(ctx context.Context) error {
			pg, err = fetchPage(ctx, l.c, opIssues, issuesVars{pageVars{First: 10}, &filter, sort, false}, wireIssue.typed)
			return nil // the answer is the measure, failing or not
		})
		return pg, err
	}
	// Logged whatever the answer: the client refuses a non-UUID before
	// sending either way, so the answer changes no client behavior.
	idPage, err := raw("ListIssues raw id in a non-UUID", issueFilterInput{ID: idIn([]string{p.a.Identifier})})
	switch e, ok := errors.AsType[*Error](err); {
	case ok && len(e.Errors) > 0:
		l.measure("id: {in: [%q]} answers an error: HTTP %d, code %s, message %q", p.a.Identifier, e.Status, e.Errors[0].Code, e.Errors[0].Message)
	case err != nil:
		l.measure("id: {in: [%q]} answers an error of type %T: %v", p.a.Identifier, err, err)
	default:
		l.measure("id: {in: [%q]} answers no error and %d issues %v (a's ID %s)", p.a.Identifier, len(idPage.Items), idsOf(idPage.Items), p.a.ID)
	}
	pg, err := raw("ListIssues raw lowercase team key", issueFilterInput{
		Team: &entityFilter{Key: &keyCompare{EqIgnoreCase: strings.ToLower(team.Key)}}, Number: &numberComparator{In: []int{p.a.Number}}})
	l.measure("team: {key: {eqIgnoreCase: %q}} with number %d matched %d issues (error %v)", strings.ToLower(team.Key), p.a.Number, len(pg.Items), err)
}

// checkSearch waits, bounded, for a's word to be searchable and reports
// how long it took; a word that never appears is reported, not failed.
func (l *live) checkSearch(team Team, p *pair) {
	start := time.Now()
	deadline := start.Add(searchWait)
	for attempt := 1; ; attempt++ {
		found := false
		l.step(fmt.Sprintf("SearchIssues %d", attempt), func(ctx context.Context) error {
			pg, err := l.c.SearchIssues(ctx, SearchQuery{Term: p.word, TeamID: team.ID, Limit: 10}, "")
			if err == nil {
				found = slices.Contains(idsOf(pg.Items), p.a.ID)
				if found && len(pg.Items) != 1 {
					err = fmt.Errorf("the unique word matched %d issues", len(pg.Items))
				}
			}
			return err
		})
		if found {
			l.measure("a new title was searchable %s after the create (%d searches)", time.Since(p.bornAt).Round(time.Second), attempt)
			return
		}
		if time.Now().Add(searchPoll).After(deadline) {
			l.measure("a new title was not searchable within %s of the first search (%d searches)", searchWait, attempt)
			return
		}
		<-time.After(searchPoll)
	}
}

// checkHistories makes the late change on both issues (state, assignee to
// the viewer, estimate when the team estimates) and reads both histories
// in one request. It asserts only what is measured: the call succeeds,
// names both issues and decodes every entry. Whether the changes are
// entries yet is logged, not asserted, until TestLiveHistoryFold settles
// when Linear writes them.
func (l *live) checkHistories(team Team, viewer *Viewer, states []WorkflowState, p *pair) {
	estimates := team.Estimation.Scale != EstimateNotUsed && team.Estimation.Scale != ""
	if !estimates {
		l.t.Logf("IssueHistories: no estimate change, since team %s does not use estimates", team.Key)
	}
	for _, issue := range []*Issue{p.a, p.b} {
		i := slices.IndexFunc(states, func(s WorkflowState) bool { return s.Type == "started" && s.ID != issue.StateID })
		if i < 0 {
			l.t.Errorf("team %s has no started state to move %s to", team.Key, issue.Identifier)
			return
		}
		in := UpdateIssueInput{StateID: Set(states[i].ID), AssigneeID: Set(viewer.User.ID)}
		if estimates {
			in.Estimate = Set(2)
		}
		if !l.step("UpdateIssue late change "+issue.Identifier, func(ctx context.Context) (err error) {
			stored, err := l.c.UpdateIssue(ctx, issue.ID, in)
			if err == nil {
				*issue = *stored
			}
			return err
		}) {
			return
		}
	}
	l.t.Logf("late change made %s after the create returned", time.Since(p.bornAt).Round(time.Second))
	l.step("IssueHistories", func(ctx context.Context) error {
		got, err := l.c.IssueHistories(ctx, []string{p.a.ID, p.b.ID})
		if err != nil {
			return err
		}
		rule, rerr := ruleCost(opIssueHistories.text, map[string]int{"first": historyBatchIssues, "entries": historyBatchEntries})
		l.measure("IssueHistories for 2 issues was charged %d points; the documented rule gives %.1f (rule error %v)", l.c.Usage().Complexity, rule, rerr)
		var named []string
		for _, h := range got {
			named = append(named, h.IssueID)
			for _, e := range h.Entries {
				l.t.Logf("history %s %s", h.IssueID, entryString(e))
				if e.ID == "" || e.CreatedAt.IsZero() {
					return fmt.Errorf("issue %s has an entry with no ID or createdAt: %s", h.IssueID, entryString(e))
				}
			}
			if h.IssueID == p.a.ID {
				has := func(match func(HistoryEntry) bool) bool { return slices.ContainsFunc(h.Entries, match) }
				l.measure("read in the second of the late change, a's history holds the immediate priority change: %t, the late state change: %t (%d entries)",
					has(func(e HistoryEntry) bool { return e.Priority != nil }), has(func(e HistoryEntry) bool { return e.State != nil }), len(h.Entries))
			}
		}
		if !sameSet(named, []string{p.a.ID, p.b.ID}) {
			return fmt.Errorf("the histories name %v, want both issues", named)
		}
		return nil
	})
}

// entryString is an entry's ID, times and the changes it carries.
func entryString(e HistoryEntry) string {
	var fields []string
	add := func(name string, set bool, v any) {
		if set {
			fields = append(fields, fmt.Sprintf("%s=%v", name, v))
		}
	}
	add("state", e.State != nil, e.State)
	add("assignee", e.Assignee != nil, e.Assignee)
	add("cycle", e.Cycle != nil, e.Cycle)
	add("project", e.Project != nil, e.Project)
	add("parent", e.Parent != nil, e.Parent)
	add("team", e.Team != nil, e.Team)
	add("estimate", e.Estimate != nil, numbers(e.Estimate))
	add("priority", e.Priority != nil, numbers(e.Priority))
	add("title", e.Title != nil, e.Title)
	add("labels", len(e.AddedLabelIDs)+len(e.RemovedLabelIDs) > 0, [][]string{e.AddedLabelIDs, e.RemovedLabelIDs})
	add("archived", e.Archived != nil, e.Archived)
	return fmt.Sprintf("entry %s createdAt %s updatedAt %s actor %s fields [%s]", e.ID,
		e.CreatedAt.Format(time.RFC3339Nano), e.UpdatedAt.Format(time.RFC3339Nano), e.ActorID, strings.Join(fields, " "))
}

func numbers(c *NumberChange) string {
	if c == nil {
		return ""
	}
	side := func(v *float64) string {
		if v == nil {
			return "none"
		}
		return strconv.FormatFloat(*v, 'g', -1, 64)
	}
	return side(c.From) + ">" + side(c.To)
}

// checkEqualOrder gives b a's sortOrder and reads their manual order
// twice.
func (l *live) checkEqualOrder(team Team, p *pair) {
	var w issueInput
	_ = OrderSort.set(&w, p.a.SortOrder) // OrderSort takes any value
	if !l.step("UpdateIssue equal sortOrder", func(ctx context.Context) error {
		_, err := l.c.updateIssue(ctx, p.b.ID, w)
		return err
	}) {
		return
	}
	var orders []string
	for read := 1; read <= 2; read++ {
		l.step(fmt.Sprintf("ListIssues equal sortOrder %d", read), func(ctx context.Context) error {
			pg, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{IDs: []string{p.a.ID, p.b.ID}}, Order: OrderManual}, "")
			var parts []string
			for _, is := range pg.Items {
				parts = append(parts, fmt.Sprintf("%s(%v)", is.Identifier, is.SortOrder))
			}
			orders = append(orders, strings.Join(parts, " "))
			return err
		})
	}
	l.measure("two issues given sortOrder %v read in manual order as [%s]", p.a.SortOrder, strings.Join(orders, "] then ["))
}

// checkArchivedAndTrashed archives a and trashes b, then reports whether
// each appears in a poll and in a team listing without includeArchived,
// and with what archivedAt and trashed; a is unarchived after, so its
// cleanup runs as for every other issue.
func (l *live) checkArchivedAndTrashed(team Team, since time.Time, p *pair) {
	if !l.step("ArchiveIssue a", func(ctx context.Context) error { _, err := l.c.ArchiveIssue(ctx, p.a.ID); return err }) {
		return
	}
	defer l.step("UnarchiveIssue a", func(ctx context.Context) error { _, err := l.c.UnarchiveIssue(ctx, p.a.ID); return err })
	if !l.step("DeleteIssue b to the trash", func(ctx context.Context) error { return l.c.DeleteIssue(ctx, p.b.ID, false) }) {
		return
	}
	p.trashed = true
	describe := func(issues []Issue) string {
		var out []string
		for _, is := range issues {
			name := map[string]string{p.a.ID: "archived", p.b.ID: "trashed"}[is.ID]
			if name != "" {
				out = append(out, fmt.Sprintf("the %s issue (archivedAt %v, trashed %t)", name, is.ArchivedAt, is.Trashed))
			}
		}
		if len(out) == 0 {
			return "neither"
		}
		return strings.Join(out, " and ")
	}
	l.step("PollIssues without archived", func(ctx context.Context) error {
		q := PollQuery{TeamID: team.ID, From: PollCursor{Since: since}}
		var all []Issue
		for {
			pg, err := l.c.PollIssues(ctx, q)
			if err != nil {
				return err
			}
			all = append(all, pg.Items...)
			if !pg.HasNextPage {
				break
			}
			q.From = pg.Next
		}
		l.measure("PollIssues without includeArchived delivers %s", describe(all))
		return nil
	})
	for _, archived := range []bool{false, true} {
		l.step(fmt.Sprintf("ListIssues includeArchived %t", archived), func(ctx context.Context) error {
			pg, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{IDs: []string{p.a.ID, p.b.ID}}, IncludeArchived: archived}, "")
			if err == nil {
				l.measure("ListIssues by ID with includeArchived %t answers %s", archived, describe(pg.Items))
			}
			return err
		})
	}
}

// ruleCost is a document's cost by Linear's documented rule as read
// here: a property is 0.1, an object 1 plus its children, and a field
// with a first argument multiplies its children's points by it (50 when
// absent on a connection is not modeled: every document here sets first).
// vars are the Int variables the document's first arguments name.
func ruleCost(text string, vars map[string]int) (float64, error) {
	doc, err := parser.ParseQuery(&ast.Source{Input: text})
	if err != nil {
		return 0, err
	}
	if len(doc.Operations) != 1 {
		return 0, fmt.Errorf("%d operations", len(doc.Operations))
	}
	var walk func(ast.SelectionSet) (float64, error)
	walk = func(set ast.SelectionSet) (float64, error) {
		total := 0.0
		for _, sel := range set {
			var inner ast.SelectionSet
			switch s := sel.(type) {
			case *ast.FragmentSpread:
				f := doc.Fragments.ForName(s.Name)
				if f == nil {
					return 0, fmt.Errorf("no fragment %s", s.Name)
				}
				inner = f.SelectionSet
			case *ast.InlineFragment:
				inner = s.SelectionSet
			case *ast.Field:
				if len(s.SelectionSet) == 0 {
					total += 0.1
					continue
				}
				children, err := walk(s.SelectionSet)
				if err != nil {
					return 0, err
				}
				n := 1
				if a := s.Arguments.ForName("first"); a != nil {
					if n, err = intArg(a.Value, vars); err != nil {
						return 0, err
					}
				}
				total += 1 + float64(n)*children
				continue
			}
			c, err := walk(inner)
			if err != nil {
				return 0, err
			}
			total += c
		}
		return total, nil
	}
	return walk(doc.Operations[0].SelectionSet)
}

func intArg(v *ast.Value, vars map[string]int) (int, error) {
	if v.Kind == ast.Variable {
		n, ok := vars[v.Raw]
		if !ok {
			return 0, fmt.Errorf("no value for $%s", v.Raw)
		}
		return n, nil
	}
	return strconv.Atoi(v.Raw)
}
