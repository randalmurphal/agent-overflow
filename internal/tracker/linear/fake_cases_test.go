package linear

import (
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker/trackerapi"
)

var linearCases = map[string]func(t *testing.T, f *conformanceEnv){
	"Viewer": func(t *testing.T, f *conformanceEnv) {
		v := answered(f.c.Viewer(f.ctx)).must(t)
		want(t, v.User.ID == cfUser1 && v.User.IsMe && v.Organization.URLKey == "example", "viewer %+v", v)
	},
	"Usage": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.Teams(f.ctx)).must(t)
		u := f.c.Usage()
		want(t, u.Requests.Known && u.Requests.Remaining > 0 && u.Points.Known, "usage %+v", u)
	},
	"Close": func(t *testing.T, f *conformanceEnv) {
		f.c.Close()
		_, err := f.c.Teams(f.ctx)
		want(t, err != nil, "a closed client answered")
	},
	"ForgetCredential": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.Teams(f.ctx)).must(t)
		f.c.ForgetCredential()
		answered(f.c.Teams(f.ctx)).must(t)
	},
	"Teams": func(t *testing.T, f *conformanceEnv) {
		ts := answered(f.c.Teams(f.ctx)).must(t)
		want(t, len(ts) == 1 && ts[0].Key == "WEB" && ts[0].ActiveCycleID == cfActive && ts[0].DefaultStateID == cfTodo, "teams %+v", ts)
	},
	"Team": func(t *testing.T, f *conformanceEnv) {
		tm := answered(f.c.Team(f.ctx, cfTeam)).must(t)
		want(t, tm.Key == "WEB" && tm.CyclesEnabled && tm.CycleWeeks == 2, "team %+v", tm)
	},
	"TeamsByKey": func(t *testing.T, f *conformanceEnv) {
		ts := answered(f.c.TeamsByKey(f.ctx, []string{"WEB"})).must(t)
		want(t, len(ts) == 1 && ts[0].ID == cfTeam, "teams %+v", ts)
	},
	"TeamMembers": func(t *testing.T, f *conformanceEnv) {
		us := answered(f.c.TeamMembers(f.ctx, cfTeam)).must(t)
		want(t, len(us) == 2, "members %+v", us)
	},
	"Users": func(t *testing.T, f *conformanceEnv) {
		active := answered(f.c.Users(f.ctx, false)).must(t)
		all := answered(f.c.Users(f.ctx, true)).must(t)
		want(t, len(active) == 2 && len(all) == 3, "users %d active, %d in all", len(active), len(all))
	},
	"WorkflowStates": func(t *testing.T, f *conformanceEnv) {
		ss := answered(f.c.WorkflowStates(f.ctx, cfTeam)).must(t)
		want(t, len(ss) == 5 && slices.ContainsFunc(ss, func(s WorkflowState) bool { return s.ID == cfDone && s.Type == StateCompleted }), "states %+v", ss)
	},
	"Cycles": func(t *testing.T, f *conformanceEnv) {
		all := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam})).must(t)
		active := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesActive})).must(t)
		upcoming := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesUpcoming})).must(t)
		past := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesPast})).must(t)
		want(t, len(all) == 3 && len(active) == 1 && active[0].ID == cfActive && active[0].IsActive &&
			len(upcoming) == 1 && upcoming[0].IsNext && len(past) == 1 && past[0].IsPrevious,
			"cycles %d, active %+v, upcoming %+v, past %+v", len(all), active, upcoming, past)
	},
	"TeamBoard": func(t *testing.T, f *conformanceEnv) {
		all := answered(f.c.TeamBoard(f.ctx, cfTeam, time.Time{})).must(t)
		kept := answered(f.c.TeamBoard(f.ctx, cfTeam, cfStart.Add(-5*cfDay))).must(t)
		want(t, all.Team.ID == cfTeam && len(all.States) == 5 && len(all.Cycles) == 3 && len(all.Users) == 3 &&
			len(kept.Cycles) == 2 && !slices.ContainsFunc(kept.Cycles, func(c Cycle) bool { return c.ID == cfPast }),
			"board %+v, kept cycles %+v", all, kept.Cycles)
		_, err := f.c.TeamBoard(f.ctx, "team-gone", time.Time{})
		want(t, errors.Is(err, trackerapi.ErrNotFound), "an invisible team: %v", err)
	},
	"Cycle": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.Cycle(f.ctx, cfUpcoming)).must(t)
		want(t, c.Number == 3 && c.IsFuture && c.TeamID == cfTeam, "cycle %+v", c)
	},
	"Labels": func(t *testing.T, f *conformanceEnv) {
		ls := answered(f.c.Labels(f.ctx, cfTeam)).must(t)
		want(t, slices.ContainsFunc(ls, func(l Label) bool { return l.ID == cfBug && l.TeamID == cfTeam }), "labels %+v", ls)
	},
	"LabelsByID": func(t *testing.T, f *conformanceEnv) {
		ls := answered(f.c.LabelsByID(f.ctx, []string{cfFeature})).must(t)
		want(t, len(ls) == 1 && ls[0].Name == "Feature" && ls[0].TeamID == "", "labels %+v", ls)
	},
	"Projects": func(t *testing.T, f *conformanceEnv) {
		ps := answered(f.c.Projects(f.ctx, cfTeam, false)).must(t)
		want(t, len(ps) == 1 && ps[0].Name == "Website", "projects %+v", ps)
	},
	"Issue": func(t *testing.T, f *conformanceEnv) {
		d := answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, d.ID == cfIssue1 && d.ProjectName == "Website" && len(d.Children) == 1 && d.Children[0].Identifier == "WEB-3" &&
			len(d.Comments.Items) == 1 && len(d.Attachments) == 1 && len(d.Relations) == 1 &&
			d.Estimate != nil && *d.Estimate == 3 && d.Priority == PriorityHigh && slices.Equal(d.LabelIDs, []string{cfBug}) &&
			len(d.History.Items) == 1 && !d.History.HasNextPage && d.History.Served.Equal(cfStart) && len(d.States) == 5 && !d.MoreStates,
			"issue %+v", d)
		// Past the first page of relations each way, the detail reads the
		// rest with IssueRelations and IssueInverseRelations.
		ws := conformanceWorkspace()
		for i := range 30 {
			ws.Relations = append(ws.Relations,
				trackerfake.LinearRelation{ID: fid(1000 + i), Type: "related", Issue: cfIssue1, Related: cfIssue3},
				trackerfake.LinearRelation{ID: fid(2000 + i), Type: "blocks", Issue: cfIssue2, Related: cfIssue1})
		}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		d = answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, len(d.Relations) == 61, "relations %d", len(d.Relations))
	},
	"IssueDetails": func(t *testing.T, f *conformanceEnv) {
		// An ID naming no issue is left out; each issue reads as Issue
		// reads it.
		ds := answered(f.c.IssueDetails(f.ctx, []string{cfIssue1, cfIssue2, fid(999)})).must(t)
		byID := map[string]*IssueDetail{}
		for _, d := range ds {
			byID[d.ID] = d
		}
		one := answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, len(ds) == 2 && byID[cfIssue2] != nil && byID[cfIssue1] != nil &&
			len(byID[cfIssue1].Comments.Items) == len(one.Comments.Items) && len(byID[cfIssue1].Relations) == len(one.Relations) &&
			len(byID[cfIssue1].Children) == 1 && len(byID[cfIssue1].States) == 5, "details %+v", ds)
		// Past the first page of relations, each issue's rest is read.
		ws := conformanceWorkspace()
		for i := range 30 {
			ws.Relations = append(ws.Relations,
				trackerfake.LinearRelation{ID: fid(1000 + i), Type: "related", Issue: cfIssue1, Related: cfIssue3},
				trackerfake.LinearRelation{ID: fid(2000 + i), Type: "related", Issue: cfIssue2, Related: cfIssue3})
		}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		ds = answered(f.c.IssueDetails(f.ctx, []string{cfIssue1, cfIssue2})).must(t)
		for _, d := range ds {
			byID[d.ID] = d
		}
		want(t, len(ds) == 2 && len(byID[cfIssue1].Relations) == 31 && len(byID[cfIssue2].Relations) >= 30,
			"relations %d and %d", len(byID[cfIssue1].Relations), len(byID[cfIssue2].Relations))
		_, err := f.c.IssueDetails(f.ctx, make([]string, MaxIssueDetails+1))
		want(t, errors.Is(err, trackerapi.ErrInvalidInput), "more than MaxIssueDetails IDs: %v", err)
	},
	"IssueStates": func(t *testing.T, f *conformanceEnv) {
		scope, ss, err := f.c.IssueStates(f.ctx, "WEB-3")
		want(t, err == nil && scope == (IssueScope{IssueID: cfIssue3, TeamID: cfTeam, StateID: cfStarted}) && len(ss) == 5, "scope %+v states %+v %v", scope, ss, err)
	},
	"IssueMembers": func(t *testing.T, f *conformanceEnv) {
		scope, us, err := f.c.IssueMembers(f.ctx, cfIssue1)
		want(t, err == nil && scope.TeamID == cfTeam && len(us) == 2, "scope %+v members %+v %v", scope, us, err)
	},
	"IssueLabels": func(t *testing.T, f *conformanceEnv) {
		scope, ls, err := f.c.IssueLabels(f.ctx, cfIssue1)
		ids := make([]string, 0, len(ls))
		for _, l := range ls {
			ids = append(ids, l.ID)
		}
		slices.Sort(ids)
		want(t, err == nil && scope.TeamID == cfTeam && slices.Equal(ids, []string{cfBug, cfFeature}), "scope %+v labels %v %v", scope, ids, err)
	},
	"IssueTeam": func(t *testing.T, f *conformanceEnv) {
		it := answered(f.c.IssueTeam(f.ctx, "WEB-2")).must(t)
		want(t, it.IssueID == cfIssue2 && !it.Archived && it.Team.ID == cfTeam && it.Team.Estimation.Scale == EstimateFibonacci, "issue team %+v", it)
	},
	"CyclePositions": func(t *testing.T, f *conformanceEnv) {
		q := IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}, IDs: []string{cfIssue1, cfIssue3, fid(99)}}, Limit: 3}
		c, p, err := f.c.CyclePositions(f.ctx, cfUpcoming, q)
		want(t, err == nil && c.ID == cfUpcoming && c.IsFuture && len(p.Items) == 2 && !p.HasNextPage, "cycle %+v positions %+v %v", c, p, err)
		_, _, err = f.c.CyclePositions(f.ctx, fid(99), q)
		want(t, errors.Is(err, trackerapi.ErrNotFound), "a missing cycle: %v", err)
	},
	"SetCycle": func(t *testing.T, f *conformanceEnv) {
		ws := answered(f.c.SetCycle(f.ctx, []string{cfIssue2, cfIssue3}, cfUpcoming)).must(t)
		want(t, len(ws) == 2 && ws[0].Err == nil && ws[1].Err == nil && ws[1].Issue.CycleID == cfUpcoming &&
			f.issue(t, cfIssue2).Cycle == cfUpcoming && f.issue(t, cfIssue3).Cycle == cfUpcoming, "set %+v", ws)
		// A missing issue fails the batch whole; the read after it finds
		// each issue unchanged.
		ws = answered(f.c.SetCycle(f.ctx, []string{cfIssue2, fid(99)}, cfActive)).must(t)
		want(t, len(ws) == 2 && WriteNotApplied(ws[0].Err) && WriteNotApplied(ws[1].Err) && f.issue(t, cfIssue2).Cycle == cfUpcoming, "failed batch %+v", ws)
	},
	"IssueSummary": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.IssueSummary(f.ctx, cfIssue3)).must(t)
		want(t, is.Identifier == "WEB-3" && is.ParentID == cfIssue1 && is.StartedAt != nil, "issue %+v", is)
	},
	"IssueComments": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueComments(f.ctx, cfIssue1, "")).must(t)
		want(t, len(p.Items) == 1 && p.Items[0].Body == "Looks good" && !p.HasNextPage, "comments %+v", p)
	},
	"IssueHistory": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueHistory(f.ctx, cfIssue1, "")).must(t)
		want(t, len(p.Items) == 1 && p.Items[0].State != nil && *p.Items[0].State == (Change{From: cfBacklog, To: cfTodo}), "history %+v", p)
		// Each entity an entry names comes with it; one the workspace
		// lacks is null and its ID stays.
		ws := conformanceWorkspace()
		gone := fid(900)
		ws.Issues[1].History = []trackerfake.LinearHistory{{CreatedAt: cfStart.Add(-cfDay), Actor: cfUser1,
			FromAssignee: gone, ToAssignee: cfUser2, FromCycle: cfActive, ToCycle: cfUpcoming, FromProject: gone, ToProject: cfProject,
			FromTeam: gone, ToTeam: cfTeam, FromParent: gone, ToParent: cfIssue1, AddedLabels: []string{cfBug, gone}, RemovedLabels: []string{cfFeature}}}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		p = answered(f.c.IssueHistory(f.ctx, cfIssue2, "")).must(t)
		want(t, len(p.Items) == 1, "history %+v", p)
		e := p.Items[0]
		want(t, e.FromAssignee == nil && e.Assignee.From == gone && *e.ToAssignee == (UserRef{ID: cfUser2, Name: "User 2", DisplayName: "User 2"}) &&
			*e.FromCycle == (CycleRef{ID: cfActive, Number: 2}) && e.ToCycle.Number == 3 &&
			e.FromProject == nil && *e.ToProject == (ProjectRef{ID: cfProject, Name: "Website"}) &&
			e.FromTeam == nil && *e.ToTeam == (TeamRef{ID: cfTeam, Key: "WEB", Name: "Web"}) &&
			e.FromParent == nil && *e.ToParent == (IssueKey{ID: cfIssue1, Identifier: "WEB-1"}) &&
			slices.Equal(e.AddedLabels, []LabelRef{{ID: cfBug, Name: "Bug"}}) && slices.Equal(e.AddedLabelIDs, []string{cfBug, gone}) &&
			slices.Equal(e.RemovedLabels, []LabelRef{{ID: cfFeature, Name: "Feature"}}),
			"named entry %+v", e)
	},
	"IssueHistories": func(t *testing.T, f *conformanceEnv) {
		hs := answered(f.c.IssueHistories(f.ctx, []string{cfIssue1, cfIssue2})).must(t)
		byID := map[string]IssueChanges{}
		for _, h := range hs {
			byID[h.IssueID] = h
		}
		want(t, len(byID) == 2 && len(byID[cfIssue1].Entries) == 1 && byID[cfIssue1].TeamID == cfTeam && byID[cfIssue1].Served.Equal(cfStart), "histories %+v", hs)
		// A history longer than the batch nests continues on its own.
		ws := conformanceWorkspace()
		for i := range historyPage + 1 {
			ws.Issues[1].History = append(ws.Issues[1].History, trackerfake.LinearHistory{
				CreatedAt: cfStart.Add(-cfDay + time.Duration(i)*time.Minute), Actor: cfUser1, FromAssignee: cfUser1, ToAssignee: cfUser2})
		}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		hs = answered(f.c.IssueHistories(f.ctx, []string{cfIssue2})).must(t)
		want(t, len(hs) == 1 && len(hs[0].Entries) == historyPage+1 && *hs[0].Entries[historyPage].Assignee == (Change{From: cfUser1, To: cfUser2}),
			"histories %+v", hs)
	},
	"IssueStateSpans": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueStateSpans(f.ctx, cfIssue1, "")).must(t)
		n := len(p.Items)
		want(t, n == 2 && p.Items[n-1].StateID == cfTodo && p.Items[n-1].EndedAt == nil, "spans %+v", p)
	},
	"ListIssues": func(t *testing.T, f *conformanceEnv) {
		all := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}}}, "")).must(t)
		started := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{StateTypes: []StateType{StateStarted}}}, "")).must(t)
		children := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{Parent: Match{IDs: []string{cfIssue1}}}}, "")).must(t)
		want(t, slices.Equal(identifiers(all.Items), []string{"WEB-1", "WEB-2", "WEB-3"}) &&
			slices.Equal(identifiers(started.Items), []string{"WEB-3"}) && slices.Equal(identifiers(children.Items), []string{"WEB-3"}),
			"all %v, started %v, children %v", identifiers(all.Items), identifiers(started.Items), identifiers(children.Items))
	},
	"ListIssuePositions": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.ListIssuePositions(f.ctx, IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}}}, "")).must(t)
		want(t, len(p.Items) == 3 && p.Items[0].ID == cfIssue1 && p.Items[2].SortOrder == 300 && p.Items[1].TeamID == cfTeam, "positions %+v", p.Items)
	},
	"SearchIssues": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.SearchIssues(f.ctx, SearchQuery{Term: "login", TeamID: cfTeam}, "")).must(t)
		got := identifiers(p.Items)
		slices.Sort(got)
		want(t, slices.Equal(got, []string{"WEB-1", "WEB-3"}), "search %v", got)
	},
	"CreateIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.CreateIssue(f.ctx, CreateIssueInput{
			TeamID: cfTeam, Title: "New page", Description: "Body", AssigneeID: Set(cfUser2), Priority: Set(PriorityLow),
			Estimate: Set(2), LabelIDs: []string{cfFeature}, CycleID: Set(cfActive), ParentID: Set("WEB-2"), DueDate: Set(Date("2026-11-30")),
		})).must(t)
		stored := f.issue(t, is.ID)
		want(t, is.Identifier == "WEB-4" && is.StateID == cfTodo && stored.Assignee == cfUser2 && stored.Parent == cfIssue2 &&
			stored.DueDate == "2026-11-30" && stored.Cycle == cfActive && slices.Equal(stored.Labels, []string{cfFeature}),
			"created %+v, stored %+v", is, stored)
	},
	"UpdateIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{
			Title: Set("Add site search"), StateID: Set(cfStarted), AssigneeID: Set(cfUser1), AddedLabelIDs: []string{cfBug}, ProjectID: Set(cfProject),
		})).must(t)
		stored := f.issue(t, cfIssue2)
		want(t, is.Title == "Add site search" && is.StartedAt != nil && stored.State == cfStarted && stored.Project == cfProject &&
			slices.Equal(stored.Labels, []string{cfBug}) && len(stored.History) == 1 && stored.History[0].ToState == cfStarted,
			"updated %+v, stored %+v", is, stored)
		cleared := answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{AssigneeID: Null[string](), ProjectID: Null[string]()})).must(t)
		want(t, cleared.AssigneeID == "" && cleared.ProjectID == "", "cleared %+v", cleared)
	},
	"ArchiveIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.ArchiveIssue(f.ctx, cfIssue2)).must(t)
		want(t, is.ArchivedAt != nil && !f.issue(t, cfIssue2).ArchivedAt.IsZero(), "archived %+v", is)
	},
	"UnarchiveIssue": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.ArchiveIssue(f.ctx, cfIssue2)).must(t)
		is := answered(f.c.UnarchiveIssue(f.ctx, cfIssue2)).must(t)
		want(t, is.ArchivedAt == nil && f.issue(t, cfIssue2).ArchivedAt.IsZero(), "unarchived %+v", is)
	},
	"DeleteIssue": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteIssue(f.ctx, cfIssue2, false); err != nil {
			t.Fatal(err)
		}
		want(t, f.issue(t, cfIssue2).Trashed, "not trashed")
		if err := f.c.DeleteIssue(f.ctx, cfIssue2, true); err != nil {
			t.Fatal(err)
		}
		want(t, !slices.ContainsFunc(f.state().Issues, func(is trackerfake.LinearIssue) bool { return is.ID == cfIssue2 }), "not deleted")
	},
	"MoveIssue": func(t *testing.T, f *conformanceEnv) {
		before, after := 100.0, 200.0
		is := answered(f.c.MoveIssue(f.ctx, MoveInput{IssueID: cfIssue3, Field: OrderSort, Before: &before, After: &after})).must(t)
		want(t, is.SortOrder > before && is.SortOrder < after && f.issue(t, cfIssue3).SortOrder == is.SortOrder, "moved %+v", is)
	},
	"RenumberIssues": func(t *testing.T, f *conformanceEnv) {
		after := 300.0
		ws := answered(f.c.RenumberIssues(f.ctx, RenumberInput{Field: OrderSort, IssueIDs: []string{cfIssue2, cfIssue1}, After: &after})).must(t)
		want(t, len(ws) == 2 && ws[0].Err == nil && ws[1].Err == nil && ws[0].Issue.SortOrder < ws[1].Issue.SortOrder && ws[1].Issue.SortOrder < after &&
			f.issue(t, cfIssue1).SortOrder == ws[1].Issue.SortOrder, "renumbered %+v", ws)
		// A missing issue at one alias fails the document whole; the read
		// after it finds the other issue unchanged.
		ws = answered(f.c.RenumberIssues(f.ctx, RenumberInput{Field: OrderSort, IssueIDs: []string{cfIssue3, fid(99)}, Before: &after})).must(t)
		want(t, len(ws) == 2 && WriteNotApplied(ws[0].Err) && WriteNotApplied(ws[1].Err) && f.issue(t, cfIssue3).SortOrder == 300, "failed renumber %+v", ws)
	},
	"PollIssues": func(t *testing.T, f *conformanceEnv) {
		first := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam})).must(t)
		for first.HasNextPage {
			first = answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: first.Next})).must(t)
		}
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue3, UpdateIssueInput{Title: Set("Login tests, all browsers")})).must(t)
		delta := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: first.Next.Resume()})).must(t)
		// Resumed exactly, the poll holds exactly the issue that changed,
		// and the walk's start is the fake's clock.
		want(t, slices.Equal(identifiers(delta.Items), []string{"WEB-3"}) && delta.Next.Walk.Start.Equal(cfStart.Add(time.Minute)),
			"delta %v walk %+v", identifiers(delta.Items), delta.Next.Walk)
		// An issue the cursor leaves out by ID comes back once updated
		// again, though no newer issue moved the floor.
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue3, UpdateIssueInput{Title: Set("Login tests, every browser")})).must(t)
		again := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: delta.Next})).must(t)
		want(t, slices.Equal(identifiers(again.Items), []string{"WEB-3"}), "updated again %v", identifiers(again.Items))
	},
	"RecentIssues": func(t *testing.T, f *conformanceEnv) {
		q := RecentQuery{Filter: IssueFilter{Teams: []string{cfTeam}}, Limit: 2}
		first := answered(f.c.RecentIssues(f.ctx, q)).must(t)
		want(t, slices.Equal(identifiers(first.Items), []string{"WEB-1", "WEB-2"}) && first.HasNextPage &&
			first.Next.Bound.Equal(f.issue(t, cfIssue2).UpdatedAt) && slices.Equal(first.Next.At, []string{cfIssue2}),
			"first page %v next %+v", identifiers(first.Items), first.Next)
		// The page's last issue is updated between pages: it moves to the
		// front, behind the walk, and the issue after it is neither
		// skipped nor joined by one delivered already.
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{Title: Set("Add site search")})).must(t)
		q.From = first.Next
		rest := answered(f.c.RecentIssues(f.ctx, q)).must(t)
		want(t, slices.Equal(identifiers(rest.Items), []string{"WEB-3"}) && !rest.HasNextPage, "rest %v", identifiers(rest.Items))
	},
	"Comment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.Comment(f.ctx, cfComment)).must(t)
		want(t, c.IssueID == cfIssue1 && c.UserID == cfUser2 && c.Body == "Looks good", "comment %+v", c)
	},
	"CreateComment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.CreateComment(f.ctx, CreateCommentInput{IssueID: "WEB-1", Body: "Thanks", ParentID: cfComment})).must(t)
		want(t, c.IssueID == cfIssue1 && c.ParentID == cfComment && c.UserID == cfUser1 && len(f.state().Comments) == 2, "comment %+v", c)
	},
	"UpdateComment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.UpdateComment(f.ctx, cfComment, "Looks great")).must(t)
		want(t, c.Body == "Looks great" && c.EditedAt != nil && f.state().Comments[0].Body == "Looks great", "comment %+v", c)
	},
	"DeleteComment": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteComment(f.ctx, cfComment); err != nil {
			t.Fatal(err)
		}
		want(t, len(f.state().Comments) == 0, "not deleted")
	},
	"Relation": func(t *testing.T, f *conformanceEnv) {
		r := answered(f.c.Relation(f.ctx, cfRel)).must(t)
		want(t, r.Type == RelationBlocks && r.Issue.Identifier == "WEB-1" && r.Related.Identifier == "WEB-2", "relation %+v", r)
	},
	"CreateRelation": func(t *testing.T, f *conformanceEnv) {
		r := answered(f.c.CreateRelation(f.ctx, CreateRelationInput{IssueID: "WEB-2", RelatedIssueID: "WEB-3", Type: RelationRelated})).must(t)
		want(t, r.Issue.ID == cfIssue2 && r.Related.ID == cfIssue3 && len(f.state().Relations) == 2, "relation %+v", r)
	},
	"DeleteRelation": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteRelation(f.ctx, cfRel); err != nil {
			t.Fatal(err)
		}
		want(t, len(f.state().Relations) == 0, "not deleted")
	},
	"LinkURL": func(t *testing.T, f *conformanceEnv) {
		a := answered(f.c.LinkURL(f.ctx, LinkInput{IssueID: "WEB-2", URL: "https://example.com/pr/2", Title: "Pull request 2", Subtitle: "open"})).must(t)
		want(t, a.IssueID == cfIssue2 && a.Subtitle == "open" && len(f.state().Attachments) == 2, "attachment %+v", a)
		again := answered(f.c.LinkURL(f.ctx, LinkInput{IssueID: "WEB-2", URL: "https://example.com/pr/2", Title: "Pull request 2", Subtitle: "merged"})).must(t)
		want(t, again.ID == a.ID && again.Subtitle == "merged" && len(f.state().Attachments) == 2, "relinked %+v", again)
	},
	"StartUpcomingCycle": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.StartUpcomingCycle(f.ctx, cfUpcoming)).must(t)
		want(t, c.IsActive && !c.StartsAt.After(cfStart), "started %+v", c)
		active := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesActive})).must(t)
		want(t, len(active) == 1 && active[0].ID == cfUpcoming, "active %+v", active)
		want(t, f.issue(t, cfIssue1).Cycle == cfUpcoming, "an unfinished issue did not roll over")
	},
}
