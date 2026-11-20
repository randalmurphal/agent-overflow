package jira

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker/adf"
)

// issueCases are the conformance cases of the platform API: users,
// fields, projects, issues, comments, links, attachments, search and
// changelogs.
var issueCases = map[string]func(t *testing.T, f *fakeEnv){
	"Myself": func(t *testing.T, f *fakeEnv) {
		if u := got(f.c.Myself(f.ctx)).must(t); u.AccountID != "acc-user1" {
			t.Fatalf("myself is %+v", u)
		}
	},
	"Me": func(t *testing.T, f *fakeEnv) {
		for range 2 {
			if u := got(f.c.Me(f.ctx)).must(t); u.AccountID != "acc-user1" {
				t.Fatalf("me is %+v", u)
			}
		}
		if n := len(f.e.Requests(0).Requests); n != 1 {
			t.Fatalf("two calls of Me sent %d requests, want 1", n)
		}
	},
	"IssueDetail": func(t *testing.T, f *fakeEnv) {
		// More changes than an issue read embeds, so the older ones are
		// read from the changelog and joined, oldest first.
		site := conformanceSite(f.clock.Now())
		is := &site.Issues[0]
		from := is.Changelog[0].Created
		for i := range 130 {
			is.Changelog = append(is.Changelog, trackerfake.JiraChange{Author: "acc-user1", Created: from.Add(time.Duration(i+1) * time.Minute),
				Items: []trackerfake.JiraChangeItem{{FieldID: "summary", Field: "summary", FieldType: "jira", ToString: ptr(strconv.Itoa(i))}}})
		}
		check(t, f.e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}))
		before := len(f.e.Requests(0).Requests)
		d := got(f.c.IssueDetail(f.ctx, "WEB-1")).must(t)
		if d.Issue.Summary != "First story" || len(d.Issue.Field("comment")) != 0 || len(d.Issue.Field("worklog")) != 0 || len(d.Transitions) == 0 || !d.Changelog.Complete {
			t.Fatalf("detail %+v", d)
		}
		got(d.Edit.Field("Area")).must(t)
		if e := d.Changelog.Entries; len(e) != 131 || !slices.IsSortedFunc(e, func(a, b ChangeEntry) int { return a.Created.Compare(b.Created) }) {
			t.Fatalf("%d changelog entries, oldest first: %v", len(e), slices.IsSortedFunc(e, func(a, b ChangeEntry) int { return a.Created.Compare(b.Created) }))
		}
		if n := len(f.e.Requests(0).Requests) - before; n != 2 {
			t.Fatalf("detail sent %d requests, want the issue read and one older changelog page", n)
		}
	},
	"LatestComments": func(t *testing.T, f *fakeEnv) {
		for _, text := range []string{"second", "third"} {
			got(f.c.AddComment(f.ctx, CommentInput{Issue: "WEB-1", Markdown: text, Audience: AudiencePublic})).must(t)
		}
		p := got(f.c.LatestComments(f.ctx, "WEB-1", 2)).must(t)
		if p.Total != 3 || p.StartAt != 1 || len(p.Comments) != 2 || !strings.Contains(adf.ToMarkdown(p.Comments[1].Body), "third") {
			t.Fatalf("latest comments %+v", p)
		}
	},
	"User": func(t *testing.T, f *fakeEnv) {
		if u := got(f.c.User(f.ctx, "acc-user2")).must(t); u.DisplayName != "User 2" {
			t.Fatalf("user is %+v", u)
		}
	},
	"SearchUsers": func(t *testing.T, f *fakeEnv) {
		if us := got(f.c.SearchUsers(f.ctx, "user", 0)).must(t); len(us) != 2 {
			t.Fatalf("found %d users", len(us))
		}
	},
	"AssignableUsers": func(t *testing.T, f *fakeEnv) {
		if us := got(f.c.AssignableUsers(f.ctx, AssignableQuery{Issue: "WEB-1", Query: "User 2"})).must(t); len(us) != 1 || us[0].AccountID != "acc-user2" {
			t.Fatalf("assignable %+v", us)
		}
	},
	"FindUser": func(t *testing.T, f *fakeEnv) {
		if u := got(f.c.FindUser(f.ctx, "User 2")).must(t); u.AccountID != "acc-user2" {
			t.Fatalf("found %+v", u)
		}
	},
	"ForgetUsers":      func(t *testing.T, f *fakeEnv) { f.c.ForgetUsers() },
	"ForgetFields":     func(t *testing.T, f *fakeEnv) { f.c.ForgetFields() },
	"ForgetBoard":      func(t *testing.T, f *fakeEnv) { f.c.ForgetBoard(1) },
	"ForgetCredential": func(t *testing.T, f *fakeEnv) { f.c.ForgetCredential(); got(f.c.Myself(f.ctx)).must(t) },
	"Close": func(t *testing.T, f *fakeEnv) {
		f.c.Close()
		if _, err := f.c.Myself(f.ctx); err == nil {
			t.Fatal("a closed client answered")
		}
	},
	"Fields": func(t *testing.T, f *fakeEnv) {
		cat := got(f.c.Fields(f.ctx)).must(t)
		if sp, ok := cat.Field("customfield_10016"); !ok || sp.Name == "" {
			t.Fatal("no story points field")
		}
	},
	"Projects": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.Projects(f.ctx, ProjectQuery{})).must(t); len(l.Projects) != 2 || !l.Complete {
			t.Fatalf("projects %+v", l)
		}
	},
	"Project": func(t *testing.T, f *fakeEnv) {
		if p := got(f.c.Project(f.ctx, "WEB")).must(t); p.Key != "WEB" || len(p.IssueTypes) == 0 {
			t.Fatalf("project %+v", p)
		}
	},
	"ProjectStatuses": func(t *testing.T, f *fakeEnv) {
		if ts := got(f.c.ProjectStatuses(f.ctx, "WEB")).must(t); len(ts) == 0 || len(ts[0].Statuses) != 3 {
			t.Fatalf("statuses %+v", ts)
		}
	},
	"Statuses":       func(t *testing.T, f *fakeEnv) { got(f.c.Statuses(f.ctx)).must(t) },
	"Priorities":     func(t *testing.T, f *fakeEnv) { got(f.c.Priorities(f.ctx)).must(t) },
	"IssueLinkTypes": func(t *testing.T, f *fakeEnv) { got(f.c.IssueLinkTypes(f.ctx)).must(t) },
	"CreateIssueTypes": func(t *testing.T, f *fakeEnv) {
		if ts := got(f.c.CreateIssueTypes(f.ctx, "WEB")).must(t); len(ts) != 5 {
			t.Fatalf("types %+v", ts)
		}
	},
	"CreateMeta": func(t *testing.T, f *fakeEnv) {
		s := got(f.c.CreateMeta(f.ctx, "WEB", "10001")).must(t)
		got(s.Field("Summary")).must(t)
	},
	"EditMeta": func(t *testing.T, f *fakeEnv) {
		s := got(f.c.EditMeta(f.ctx, "WEB-1")).must(t)
		got(s.Field("Area")).must(t)
	},
	"GetIssue": func(t *testing.T, f *fakeEnv) {
		is := got(f.c.GetIssue(f.ctx, "WEB-1")).must(t)
		if is.Summary != "First story" || is.Status == nil || is.Status.ID != "10001" || len(is.Attachments) != 1 {
			t.Fatalf("issue %+v", is)
		}
	},
	"Value": func(t *testing.T, f *fakeEnv) {
		s := got(f.c.EditMeta(f.ctx, "WEB-1")).must(t)
		meta := got(s.Field("Area")).must(t)
		v := got(f.c.Value(f.ctx, meta, Text("Backend"), ValueOptions{Issue: "WEB-1"})).must(t)
		if string(v.Fields) != `{"id":"20002"}` {
			t.Fatalf("value %s", v.Fields)
		}
	},
	"PlanCreate": func(t *testing.T, f *fakeEnv) {
		got(f.c.PlanCreate(f.ctx, CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{{Field: "Summary", Value: Text("Planned")}}})).must(t)
	},
	"ApplyCreate": func(t *testing.T, f *fakeEnv) {
		p := got(f.c.PlanCreate(f.ctx, CreateInput{Project: "WEB", IssueType: "Task", Fields: []FieldInput{
			{Field: "Summary", Value: Text("Created")}, {Field: "Description", Value: Text("Some *text*")},
			{Field: "Area", Value: Text("Frontend")}, {Field: "Labels", Value: Texts("one", "two")},
		}})).must(t)
		res := got(f.c.ApplyCreate(f.ctx, p)).must(t)
		is := f.issueOf(t, res.Key)
		if is.Summary != "Created" || len(is.Labels) != 2 || !strings.Contains(string(is.Values["customfield_10001"]), "Frontend") {
			t.Fatalf("created %+v", is)
		}
	},
	"PlanEdit": func(t *testing.T, f *fakeEnv) {
		got(f.c.PlanEdit(f.ctx, EditInput{Issue: "WEB-2", Fields: []FieldInput{{Field: "Summary", Value: Text("Renamed")}}})).must(t)
	},
	"ApplyEdit": func(t *testing.T, f *fakeEnv) {
		p := got(f.c.PlanEdit(f.ctx, EditInput{Issue: "WEB-1", AcceptLoss: true, Fields: []FieldInput{
			{Field: "Summary", Value: Text("Renamed")}, {Field: "Description", Value: Text("New text")},
			{Field: "Assignee", Value: Text("User 2")}, {Field: "Story Points", Value: Text("5")},
		}})).must(t)
		res := got(f.c.ApplyEdit(f.ctx, p)).must(t)
		if res.Issue == nil || res.Issue.Summary != "Renamed" {
			t.Fatalf("edit returned %+v", res.Issue)
		}
		is := f.issueOf(t, "WEB-1")
		if is.Assignee != "acc-user2" || string(is.Values["customfield_10016"]) != "5" || len(is.Changelog) < 2 {
			t.Fatalf("edited %+v", is)
		}
	},
	"Transitions": func(t *testing.T, f *fakeEnv) {
		if ts := got(f.c.Transitions(f.ctx, "WEB-1")).must(t); len(ts) != 3 {
			t.Fatalf("transitions %+v", ts)
		}
	},
	"Transition": func(t *testing.T, f *fakeEnv) {
		res := got(f.c.Transition(f.ctx, TransitionInput{Issue: "WEB-1", Transition: "Done", Comment: "Finished"})).must(t)
		is := f.issueOf(t, "WEB-1")
		if !res.Moved || is.Status != "10002" || is.Resolution == "" || len(is.Comments) != 2 {
			t.Fatalf("moved %+v to %+v", res, is)
		}
	},
	"Assign": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.Assign(f.ctx, "WEB-2", "acc-user2"))
		if f.issueOf(t, "WEB-2").Assignee != "acc-user2" {
			t.Fatal("not assigned")
		}
	},
	"Unassign": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.Unassign(f.ctx, "WEB-1"))
		if f.issueOf(t, "WEB-1").Assignee != "" {
			t.Fatal("still assigned")
		}
	},
	"Search": func(t *testing.T, f *fakeEnv) {
		p := got(f.c.Search(f.ctx, SearchQuery{JQL: `text ~ "story" AND project = "WEB" ORDER BY updated DESC`, Fields: []string{"summary"}})).must(t)
		if len(p.Issues) != 1 || p.Issues[0].Key != "WEB-1" || p.NextPageToken != "" {
			t.Fatalf("found %d issues", len(p.Issues))
		}
	},
	"SearchAll": func(t *testing.T, f *fakeEnv) {
		r := got(f.c.SearchAll(f.ctx, SearchQuery{JQL: `updated >= 0 ORDER BY updated ASC, id ASC`, Fields: []string{"summary"}, MaxResults: 2}, 0)).must(t)
		if len(r.Issues) != 5 || !r.Complete {
			t.Fatalf("found %d issues, complete %v", len(r.Issues), r.Complete)
		}
	},
	"CountIssues": func(t *testing.T, f *fakeEnv) {
		if n := got(f.c.CountIssues(f.ctx, `key in ("WEB-1", "WEB-2", "WEB-99")`)).must(t); n != 2 {
			t.Fatalf("counted %d", n)
		}
	},
	"BulkFetch": func(t *testing.T, f *fakeEnv) {
		r := got(f.c.BulkFetch(f.ctx, []string{"WEB-1", "10102", "WEB-99"}, "summary")).must(t)
		if len(r.Issues) != 2 || r.Complete {
			t.Fatalf("fetched %+v", r)
		}
	},
	"Changelog": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.Changelog(f.ctx, "WEB-1")).must(t); len(l.Entries) != 1 || !l.Complete {
			t.Fatalf("changelog %+v", l)
		}
	},
	"BulkChangelogs": func(t *testing.T, f *fakeEnv) {
		l := got(f.c.BulkChangelogs(f.ctx, ChangelogQuery{Issues: []string{"WEB-1", "WEB-2"}, Fields: []string{"status"}})).must(t)
		if len(l.ByIssue["10101"]) != 1 || len(l.ByIssue) != 1 {
			t.Fatalf("changelogs %+v", l)
		}
	},
	"Comments": func(t *testing.T, f *fakeEnv) {
		if p := got(f.c.Comments(f.ctx, "WEB-1", 0, 0)).must(t); p.Total != 1 || len(p.Comments) != 1 {
			t.Fatalf("comments %+v", p)
		}
	},
	"Comment": func(t *testing.T, f *fakeEnv) {
		if cm := got(f.c.Comment(f.ctx, "WEB-1", "40001")).must(t); cm.ID != "40001" {
			t.Fatalf("comment %+v", cm)
		}
	},
	"AddComment": func(t *testing.T, f *fakeEnv) {
		res := got(f.c.AddComment(f.ctx, CommentInput{Issue: "WEB-2", Markdown: "Hello"})).must(t)
		if res.Comment == nil || len(f.issueOf(t, "WEB-2").Comments) != 1 {
			t.Fatalf("added %+v", res)
		}
	},
	"EditComment": func(t *testing.T, f *fakeEnv) {
		got(f.c.EditComment(f.ctx, CommentEdit{Issue: "WEB-1", ID: "40001", Markdown: "Changed", Unchecked: true})).must(t)
		if !strings.Contains(string(f.issueOf(t, "WEB-1").Comments[0].Body), "Changed") {
			t.Fatal("not edited")
		}
	},
	"DeleteComment": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.DeleteComment(f.ctx, "WEB-1", "40001"))
		if len(f.issueOf(t, "WEB-1").Comments) != 0 {
			t.Fatal("not deleted")
		}
	},
	"LinkIssues": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.LinkIssues(f.ctx, LinkInput{Type: "Blocks", Outward: "WEB-1", Inward: "WEB-2"}))
		is := got(f.c.GetIssue(f.ctx, "WEB-2", "issuelinks")).must(t)
		if len(is.Links) != 1 {
			t.Fatalf("links %+v", is.Links)
		}
	},
	"UnlinkIssues": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.LinkIssues(f.ctx, LinkInput{Type: "Blocks", Outward: "WEB-1", Inward: "WEB-2"}))
		id := f.e.Snapshot().Jira[0].Links[0].ID
		check(t, f.c.UnlinkIssues(f.ctx, id))
		if len(f.e.Snapshot().Jira[0].Links) != 0 {
			t.Fatal("still linked")
		}
	},
	"RemoteLinks": func(t *testing.T, f *fakeEnv) { got(f.c.RemoteLinks(f.ctx, "WEB-1")).must(t) },
	"AddRemoteLink": func(t *testing.T, f *fakeEnv) {
		got(f.c.AddRemoteLink(f.ctx, "WEB-1", RemoteLinkInput{URL: "https://example.com/page", Title: "Page"})).must(t)
		if ls := got(f.c.RemoteLinks(f.ctx, "WEB-1")).must(t); len(ls) != 1 {
			t.Fatalf("links %+v", ls)
		}
	},
	"RemoveRemoteLink": func(t *testing.T, f *fakeEnv) {
		id := got(f.c.AddRemoteLink(f.ctx, "WEB-1", RemoteLinkInput{URL: "https://example.com/page", Title: "Page"})).must(t)
		check(t, f.c.RemoveRemoteLink(f.ctx, "WEB-1", id))
		if len(f.issueOf(t, "WEB-1").RemoteLinks) != 0 {
			t.Fatal("still linked")
		}
	},
	"Watchers": func(t *testing.T, f *fakeEnv) {
		if w := got(f.c.Watchers(f.ctx, "WEB-1")).must(t); !w.Watching || w.Count != 1 {
			t.Fatalf("watchers %+v", w)
		}
	},
	"AddWatcher": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.AddWatcher(f.ctx, "WEB-1", "acc-user2"))
		if len(f.issueOf(t, "WEB-1").Watchers) != 2 {
			t.Fatal("not watching")
		}
	},
	"RemoveWatcher": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.RemoveWatcher(f.ctx, "WEB-1", "acc-user1"))
		if len(f.issueOf(t, "WEB-1").Watchers) != 0 {
			t.Fatal("still watching")
		}
	},
	"Attachments": func(t *testing.T, f *fakeEnv) {
		if as := got(f.c.Attachments(f.ctx, "WEB-1")).must(t); len(as) != 1 {
			t.Fatalf("attachments %+v", as)
		}
	},
	"Attach": func(t *testing.T, f *fakeEnv) {
		as := got(f.c.Attach(f.ctx, "WEB-2", File{Name: "a.txt", Content: strings.NewReader("data")})).must(t)
		if len(as) != 1 || len(f.issueOf(t, "WEB-2").Attachments) != 1 {
			t.Fatalf("attached %+v", as)
		}
	},
	"DeleteAttachment": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.DeleteAttachment(f.ctx, "60001"))
		if len(f.issueOf(t, "WEB-1").Attachments) != 0 {
			t.Fatal("not deleted")
		}
	},
	"DownloadAttachment": func(t *testing.T, f *fakeEnv) {
		var b bytes.Buffer
		check(t, f.c.DownloadAttachment(f.ctx, "60001", &b, 1<<20))
		if b.String() != "hello" {
			t.Fatalf("downloaded %q", b.String())
		}
	},
	"AttachmentMediaID": func(t *testing.T, f *fakeEnv) {
		id := got(f.c.AttachmentMediaID(f.ctx, "60001")).must(t)
		if id != f.issueOf(t, "WEB-1").Attachments[0].MediaID {
			t.Fatalf("media ID %q", id)
		}
	},
	"Boards": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.Boards(f.ctx, BoardQuery{Project: "WEB"})).must(t); len(l.Boards) != 1 {
			t.Fatalf("boards %+v", l)
		}
	},
}

// boardCases are the conformance cases of the Agile and Software APIs
// and of worklogs.
var boardCases = map[string]func(t *testing.T, f *fakeEnv){
	"Board": func(t *testing.T, f *fakeEnv) { got(f.c.Board(f.ctx, 1)).must(t) },
	"BoardConfig": func(t *testing.T, f *fakeEnv) {
		cfg := got(f.c.BoardConfig(f.ctx, 1)).must(t)
		if cfg.Estimation.FieldID != "customfield_10016" || len(cfg.Columns) != 3 {
			t.Fatalf("config %+v", cfg)
		}
	},
	"BoardSprints": func(t *testing.T, f *fakeEnv) {
		for range 2 {
			l := got(f.c.BoardSprints(f.ctx, 1)).must(t)
			if len(l.Sprints) != 2 || !l.Complete || !l.Read.Equal(f.clock.Now().Truncate(time.Second)) {
				t.Fatalf("sprints %+v", l)
			}
		}
		if n := len(f.e.Requests(0).Requests); n != 1 {
			t.Fatalf("a cached listing read again: %d requests", n)
		}
		f.c.ForgetBoard(1)
		got(f.c.BoardSprints(f.ctx, 1)).must(t)
		if n := len(f.e.Requests(0).Requests); n != 2 {
			t.Fatalf("a forgotten board's listing was not read again: %d requests", n)
		}
	},
	"Sprints": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.Sprints(f.ctx, SprintQuery{Board: 1, States: []SprintState{SprintActive}})).must(t); len(l.Sprints) != 1 {
			t.Fatalf("sprints %+v", l)
		}
	},
	"Sprint": func(t *testing.T, f *fakeEnv) {
		if s := got(f.c.Sprint(f.ctx, 1)).must(t); s.Name != "Sprint 1" {
			t.Fatalf("sprint %+v", s)
		}
	},
	"CreateSprint": func(t *testing.T, f *fakeEnv) {
		if s := got(f.c.CreateSprint(f.ctx, SprintInput{Board: 1, Name: "Sprint 3"})).must(t); s.ID == 0 || s.State != SprintFuture {
			t.Fatalf("created %+v", s)
		}
	},
	"UpdateSprint": func(t *testing.T, f *fakeEnv) {
		goal := "Ship"
		if s := got(f.c.UpdateSprint(f.ctx, SprintUpdate{ID: 2, Goal: &goal})).must(t); s.Goal != "Ship" {
			t.Fatalf("updated %+v", s)
		}
	},
	"StartSprint": func(t *testing.T, f *fakeEnv) {
		now := f.clock.Now()
		if s := got(f.c.StartSprint(f.ctx, SprintStart{ID: 2, Start: now, End: now.Add(7 * 24 * time.Hour)})).must(t); s.State != SprintActive {
			t.Fatalf("started %+v", s)
		}
	},
	"DeleteSprint": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.DeleteSprint(f.ctx, 2))
		if len(f.e.Snapshot().Jira[0].Sprints) != 1 {
			t.Fatal("not deleted")
		}
	},
	"CompleteSprint": func(t *testing.T, f *fakeEnv) {
		res := got(f.c.CompleteSprint(f.ctx, SprintCompletion{Sprint: 1, MoveTo: 2, Board: 1})).must(t)
		if len(res.Unfinished) != 2 || !slices.Contains(f.issueOf(t, "WEB-2").Sprints, 2) {
			t.Fatalf("completion %+v", res)
		}
	},
	"MoveToSprint": func(t *testing.T, f *fakeEnv) {
		got(f.c.MoveToSprint(f.ctx, SprintMove{Sprint: 2, Issues: []string{"WEB-4"}})).must(t)
		if !slices.Contains(f.issueOf(t, "WEB-4").Sprints, 2) {
			t.Fatal("not moved")
		}
	},
	"MoveToBacklog": func(t *testing.T, f *fakeEnv) {
		got(f.c.MoveToBacklog(f.ctx, BacklogMove{Issues: []string{"WEB-2"}})).must(t)
		if len(f.issueOf(t, "WEB-2").Sprints) != 0 {
			t.Fatal("not moved")
		}
	},
	"Rank": func(t *testing.T, f *fakeEnv) {
		got(f.c.Rank(f.ctx, RankInput{Issues: []string{"WEB-4"}, Position: RankBefore("WEB-1")})).must(t)
		l := got(f.c.ListIssues(f.ctx, ListQuery{List: BoardIssues(1)})).must(t)
		if l.Issues[0].Key != "WEB-4" {
			t.Fatalf("first is %s", l.Issues[0].Key)
		}
	},
	"SetEstimate": func(t *testing.T, f *fakeEnv) {
		field, stored, err := f.c.SetEstimate(f.ctx, EstimateInput{Issue: "WEB-2", Board: 1, Value: Points(8)})
		check(t, err)
		if field != "customfield_10016" || stored != "8" {
			t.Fatalf("estimate %s %s", field, stored)
		}
	},
	"ListIssues": func(t *testing.T, f *fakeEnv) {
		if p := got(f.c.ListIssues(f.ctx, ListQuery{List: BacklogIssues(1)})).must(t); len(p.Issues) != 2 {
			t.Fatalf("backlog %d", len(p.Issues))
		}
	},
	"ListAllIssues": func(t *testing.T, f *fakeEnv) {
		r := got(f.c.ListAllIssues(f.ctx, ListQuery{List: SprintIssues(1), MaxResults: 1, Fields: []string{"status"}}, 0)).must(t)
		if len(r.Issues) != 2 || !r.Complete {
			t.Fatalf("sprint issues %d", len(r.Issues))
		}
	},
	"CountListIssues": func(t *testing.T, f *fakeEnv) {
		if n := got(f.c.CountListIssues(f.ctx, BoardIssues(1), `id in (10101, 10103)`)).must(t); n != 2 {
			t.Fatalf("counted %d", n)
		}
	},
	"Worklogs": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.Worklogs(f.ctx, WorklogQuery{Issue: "WEB-1"})).must(t); len(l.Worklogs) != 1 || !l.Complete {
			t.Fatalf("worklogs %+v", l)
		}
	},
	"Worklog": func(t *testing.T, f *fakeEnv) { got(f.c.Worklog(f.ctx, "WEB-1", "50001")).must(t) },
	"AddWorklog": func(t *testing.T, f *fakeEnv) {
		w := got(f.c.AddWorklog(f.ctx, WorklogInput{Issue: "WEB-2", Started: f.clock.Now(), TimeSpent: time.Hour, Markdown: "Work", Estimate: LeaveEstimate()})).must(t)
		if w.TimeSpent != time.Hour || len(f.issueOf(t, "WEB-2").Worklogs) != 1 {
			t.Fatalf("worklog %+v", w)
		}
	},
	"UpdateWorklog": func(t *testing.T, f *fakeEnv) {
		got(f.c.UpdateWorklog(f.ctx, WorklogEdit{Issue: "WEB-1", ID: "50001", Unchecked: true, TimeSpent: 2 * time.Hour, Estimate: LeaveEstimate()})).must(t)
		if f.issueOf(t, "WEB-1").Worklogs[0].TimeSpent != 7200 {
			t.Fatal("not updated")
		}
	},
	"DeleteWorklog": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.DeleteWorklog(f.ctx, "WEB-1", "50001", AdjustAuto()))
		if len(f.issueOf(t, "WEB-1").Worklogs) != 0 {
			t.Fatal("not deleted")
		}
	},
	"UpdatedWorklogs": func(t *testing.T, f *fakeEnv) {
		ch := got(f.c.UpdatedWorklogs(f.ctx, time.UnixMilli(0))).must(t)
		if len(ch.Changes) != 1 || !ch.Complete {
			t.Fatalf("changes %+v", ch)
		}
	},
	"DeletedWorklogs": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.DeleteWorklog(f.ctx, "WEB-1", "50001", LeaveEstimate()))
		f.clock.Advance(2 * time.Minute)
		ch := got(f.c.DeletedWorklogs(f.ctx, time.UnixMilli(0))).must(t)
		if len(ch.Changes) != 1 || ch.Changes[0].ID != 50001 {
			t.Fatalf("changes %+v", ch)
		}
	},
	"WorklogsByID": func(t *testing.T, f *fakeEnv) {
		if l := got(f.c.WorklogsByID(f.ctx, []int64{50001})).must(t); len(l.Worklogs) != 1 || !l.Complete {
			t.Fatalf("worklogs %+v", l)
		}
	},
}
