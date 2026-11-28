package app

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
)

// An agent's comment on a Jira ticket is judged on the project a read
// made after the approval finds the ticket in, or just before the send
// when one ticket waits for nobody. WEB-3 moves into a service desk
// project after the plan read it (the mirror's card, for a comment
// alone): with one ticket before the call, with three while the approval
// waits. Its comment is not sent, since the agent named no audience and
// a public one would email the customer; the other tickets get theirs.
func TestTrackerAgentCommentAudienceIsReadAfterTheWait(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d tickets", n), func(t *testing.T) {
			t.Parallel()
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: audienceSite()})
			t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
			thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
			readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
			keys := []string{"WEB-1", "WEB-2", "WEB-3"}[3-n:]
			args := map[string]any{"keys": keys, "comment": "Checked."}
			var result [2]any
			if n == 1 {
				moveIntoServiceDesk(t, f, "10103")
				result = <-trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
			} else {
				done := trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
				open, _ := trackerWritePrompt(t, f.a, thread.ID)
				moveIntoServiceDesk(t, f, "10103")
				if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
					t.Fatal(err)
				}
				result = <-done
			}
			if got := fakeComments(t, f, "10103"); len(got) != 0 {
				t.Errorf("the ticket in the service desk project got %q; want no comment", got)
			}
			for _, id := range []string{"10101", "10102"}[:n-1] {
				if got := fakeComments(t, f, id); len(got) != 1 {
					t.Errorf("%s got %q; want the agent's comment", id, got)
				}
			}
			text, _ := result[0].(string)
			if !strings.Contains(text, `"key":"WEB-3","refused":`) || !strings.Contains(text, "service desk") {
				t.Errorf("ticket_update = %v; want WEB-3 refused for its service desk project", result)
			}
		})
	}
}

// audienceSite is the board site with a service desk project, SD.
func audienceSite() trackerfake.JiraSite {
	site := trackerBoardSite()
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10010", Key: "SD", Name: "Service", TypeKey: "service_desk"})
	return site
}

// moveIntoServiceDesk moves issue id of f's site into project SD, under
// key SD-1, keeping its former key.
func moveIntoServiceDesk(t *testing.T, f *trackerBoardFixture, id string) {
	t.Helper()
	if err := serviceDeskMove(f, id); err != nil {
		t.Fatal(err)
	}
}

// serviceDeskMove is moveIntoServiceDesk for a goroutine other than the
// test's, which reports its failure.
func serviceDeskMove(f *trackerBoardFixture, id string) error {
	world := f.e.Snapshot()
	s := &world.Jira[0]
	s.Token = trackerTestToken
	for i := range s.Issues {
		if is := &s.Issues[i]; is.ID == id {
			is.Project, is.Key, is.FormerKeys = "SD", "SD-1", append(is.FormerKeys, is.Key)
		}
	}
	if err := f.e.Seed(world); err != nil {
		return fmt.Errorf("move %s into SD: %w", id, err)
	}
	return nil
}

// A write session judges a comment on the project its key lookup found,
// while it sent nothing but comments on the connection since: with a
// title sent after the lookup, the comment reads the issue's project
// again, one request, and is refused once the issue is in a service desk
// project. With no write between, the comment takes the lookup's project
// and reads nothing.
func TestTrackerWriteSessionReadsACommentsProjectAgainAfterAnotherWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		write bool
	}{{"a title sent after the lookup", true}, {"nothing sent after the lookup", false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: audienceSite()})
			svc, err := f.a.trackerService()
			if err != nil {
				t.Fatal(err)
			}
			ctx := trackerapp.WithWrites(t.Context())
			t.Cleanup(func() { _ = svc.EndWrites(ctx) })
			if _, err := svc.Lookup(ctx, f.conn.ID, []string{"WEB-3"}); err != nil {
				t.Fatal(err)
			}
			if c.write {
				if _, err := svc.Edit(ctx, f.conn.ID, trackerapp.EditRequest{Issue: "10103", Fields: []trackerapp.FieldEditInput{{Field: "title", Typed: []string{"Renamed"}}}}); err != nil {
					t.Fatal(err)
				}
				moveIntoServiceDesk(t, f, "10103")
			}
			since := f.e.Requests(0).Requests
			v, err := svc.AddComment(ctx, f.conn.ID, trackerapp.CommentRequest{Issue: "10103", Markdown: "Checked."})
			if err != nil {
				t.Fatal(err)
			}
			var routes []string
			for _, r := range f.e.Requests(since[len(since)-1].Seq).Requests {
				routes = append(routes, r.Route)
			}
			reads := slices.Index(routes, "GET rest/api/3/issue/{issue}") >= 0
			if !c.write {
				if reads || len(v.Parts) != 1 || v.Parts[0].State != "applied" {
					t.Errorf("requests %q, parts %+v; want the comment added on the lookup's project, reading nothing", routes, v.Parts)
				}
				return
			}
			if !slices.Equal(routes, []string{"GET rest/api/3/issue/{issue}"}) || len(v.Parts) != 1 || v.Parts[0].State != "refused" ||
				!strings.Contains(v.Parts[0].Refusal.Message, "service desk") {
				t.Errorf("requests %q, parts %+v; want one read of the issue's project and the comment refused for its service desk", routes, v.Parts)
			}
			if got := fakeComments(t, f, "10103"); len(got) != 0 {
				t.Errorf("the ticket in the service desk project got %q", got)
			}
		})
	}
}

// A Jira comment is judged on the project the ticket is in after the
// call's other writes: WEB-3 moves into a service desk project as the
// call's own write to it arrives at Jira, its title on one ticket, its
// status on three after the approval. Its comment is not sent, and the
// other tickets get theirs.
func TestTrackerAgentCommentAudienceIsReadAfterTheCallsOtherWrites(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		keys    []string
		set     map[string]any
		method  string
		path    string
		applied string
	}{
		{"one ticket, its title", []string{"WEB-3"}, map[string]any{"title": "Renamed"}, http.MethodPut, "/rest/api/3/issue/10103", "title"},
		{"three tickets, their status", []string{"WEB-1", "WEB-2", "WEB-3"}, map[string]any{"status": "Done"}, http.MethodPost, "/rest/api/3/issue/10103/transitions", "status"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var armed *trackerBoardFixture
			var once sync.Once
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: audienceSite(), arrive: func(r *http.Request) {
				mu.Lock()
				f := armed
				mu.Unlock()
				if f != nil && r.Method == c.method && strings.HasSuffix(r.URL.Path, c.path) {
					once.Do(func() {
						if err := serviceDeskMove(f, "10103"); err != nil {
							t.Error(err)
						}
					})
				}
			}})
			t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
			thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
			readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
			mu.Lock()
			armed = f
			mu.Unlock()
			text, _ := toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": c.keys, "set": c.set, "comment": "Checked."})
			if got := fakeComments(t, f, "10103"); len(got) != 0 {
				t.Errorf("the ticket in the service desk project got %q; want no comment", got)
			}
			for _, id := range []string{"10101", "10102"}[:len(c.keys)-1] {
				if got := fakeComments(t, f, id); len(got) != 1 {
					t.Errorf("%s got %q; want the agent's comment", id, got)
				}
			}
			if !strings.Contains(text, `"key":"WEB-3","applied":["`+c.applied+`"],"refused":"comment: `) || !strings.Contains(text, "service desk") {
				t.Errorf("ticket_update = %s; want WEB-3's %s applied and its comment refused for its service desk project", text, c.applied)
			}
		})
	}
}
