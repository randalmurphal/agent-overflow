//go:build linearlive

// The manual live check against Linear's real API. It is excluded from
// every normal run by its build tag, and run by hand on the host network
// against a blank throwaway workspace:
//
//	LINEAR_LIVE_KEY_FILE=/path/to/key go test -tags linearlive -run TestLive -v -count=1 ./internal/tracker/linear
//
// It creates three issues titled "agent-overflow live check ..." and
// archives and trashes each of them when it ends, whether or not a step
// failed. It logs every call's outcome and rate limit headers, never the
// key.

package linear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// liveKeyEnv names the file holding the personal API key; the only
// environment the package's tree reads.
const liveKeyEnv = "LINEAR_LIVE_KEY_FILE"

func TestLive(t *testing.T) {
	path := os.Getenv(liveKeyEnv)
	if path == "" {
		t.Skipf("%s is not set", liveKeyEnv)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	key := secret.New(bytes.TrimSpace(raw))
	clear(raw)
	// Registered first, so the key is zeroed after every client is closed.
	t.Cleanup(key.Zero)
	creds := trackerapi.StaticCredentials(trackerapi.Key(key))
	c, err := New(Options{Version: "live-check", Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	// Registered next, so it runs after the cleanup of every issue below.
	t.Cleanup(c.Close)
	l := &live{t: t, c: c, creds: creds}

	var viewer *Viewer
	l.must("Viewer", func(ctx context.Context) (err error) { viewer, err = c.Viewer(ctx); return err })
	t.Logf("viewer %s in workspace %s", viewer.User.DisplayName, viewer.Organization.URLKey)
	var teams []Team
	l.must("Teams", func(ctx context.Context) (err error) { teams, err = c.Teams(ctx); return err })
	if len(teams) == 0 {
		t.Fatal("the workspace has no team")
	}
	team := teams[0]
	t.Logf("team %s %s estimation %+v cycles %t triage %t", team.Key, team.Name, team.Estimation, team.CyclesEnabled, team.TriageEnabled)
	var states []WorkflowState
	l.must("WorkflowStates", func(ctx context.Context) (err error) { states, err = c.WorkflowStates(ctx, team.ID); return err })
	for _, s := range states {
		t.Logf("state %s %s position %v", s.Type, s.Name, s.Position)
	}
	l.step("Cycles", func(ctx context.Context) error { _, err := c.Cycles(ctx, CycleQuery{TeamID: team.ID}); return err })
	l.step("Labels", func(ctx context.Context) error { _, err := c.Labels(ctx, team.ID); return err })
	l.step("Users", func(ctx context.Context) error { _, err := c.Users(ctx, false); return err })
	l.step("TeamMembers", func(ctx context.Context) error { _, err := c.TeamMembers(ctx, team.ID); return err })
	l.step("Projects", func(ctx context.Context) error { _, err := c.Projects(ctx, team.ID, false); return err })

	l.sweep(team)

	since := time.Now().Add(-5 * time.Minute)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	var created []*Issue
	for i := 1; i <= 3; i++ {
		var issue *Issue
		l.must(fmt.Sprintf("CreateIssue %d", i), func(ctx context.Context) (err error) {
			issue, err = c.CreateIssue(ctx, CreateIssueInput{
				TeamID: team.ID, Title: fmt.Sprintf("%s%s #%d", liveTitle, stamp, i),
				Description: "Test data from the agent-overflow live check. Safe to delete.",
			})
			return err
		})
		created = append(created, issue)
		t.Cleanup(func() { l.cleanup(issue) })
		t.Logf("created %s sortOrder %v", issue.Identifier, issue.SortOrder)
		// RenumberIssues accepts only this spelling of an issue ID.
		if !canonicalUUID(issue.ID) {
			t.Errorf("issue ID %q is not a lowercase UUID", issue.ID)
		}
	}
	ids := []string{created[0].ID, created[1].ID, created[2].ID}

	l.checkList(team, ids)
	l.checkPoll(team, since, ids)
	l.checkCursorAcrossClients(team, since, created[0])
	l.checkReorder(team, created)
	l.checkComments(created[0])
	l.checkUpdate(team, viewer, created[1])
	l.checkLinks(created)
	l.step("ArchiveIssue", func(ctx context.Context) error {
		issue, err := c.ArchiveIssue(ctx, created[2].ID)
		if err == nil && issue.ArchivedAt == nil {
			err = errors.New("the archived issue has no archivedAt")
		}
		return err
	})
	l.step("UnarchiveIssue", func(ctx context.Context) error { _, err := c.UnarchiveIssue(ctx, created[2].ID); return err })
	l.step("Issue not found", func(ctx context.Context) error {
		_, err := c.Issue(ctx, "00000000-0000-4000-8000-000000000000")
		t.Logf("a missing issue answers: %v", err)
		if !errors.Is(err, trackerapi.ErrNotFound) {
			return fmt.Errorf("a missing issue does not match ErrNotFound: %w", err)
		}
		return nil
	})
}

// live runs the check's steps, logging each.
type live struct {
	t     *testing.T
	c     *Client
	creds trackerapi.CredentialSource
}

// step runs one call and logs its outcome and the budget headers; a
// failure is reported and the check goes on.
func (l *live) step(name string, call func(context.Context) error) bool {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(trackerapi.WithInteractive(context.Background()), time.Minute)
	defer cancel()
	err := call(ctx)
	u := l.c.Usage()
	l.t.Logf("%s: complexity %d, requests %d/%d (reset %s), points %d/%d (reset %s)", name, u.Complexity,
		u.Requests.Remaining, u.Requests.Limit, u.Requests.Reset.Format(time.RFC3339),
		u.Points.Remaining, u.Points.Limit, u.Points.Reset.Format(time.RFC3339))
	if err != nil {
		l.t.Errorf("%s failed: %v", name, err)
		logBody(l.t, err)
		return false
	}
	l.t.Logf("%s: ok", name)
	return true
}

// must is step for a call the rest of the check depends on.
func (l *live) must(name string, call func(context.Context) error) {
	l.t.Helper()
	if !l.step(name, call) {
		l.t.FailNow()
	}
}

func logBody(t *testing.T, err error) {
	t.Helper()
	if e, ok := errors.AsType[*Error](err); ok {
		t.Logf("  HTTP %d, refused %t, errors %+v", e.Status, e.Refused, e.Errors)
	}
	if e, ok := errors.AsType[*trackerapi.StatusError](err); ok {
		t.Logf("  HTTP %d body: %s", e.Status, e.Body)
	}
	if e, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		t.Logf("  HTTP %d auth: %s", e.Status, e.Message)
	}
}

// cleanup archives and trashes an issue the check created.
func (l *live) cleanup(issue *Issue) {
	l.step("cleanup ArchiveIssue "+issue.Identifier, func(ctx context.Context) error { _, err := l.c.ArchiveIssue(ctx, issue.ID); return err })
	l.step("cleanup DeleteIssue "+issue.Identifier, func(ctx context.Context) error { return l.c.DeleteIssue(ctx, issue.ID, false) })
}

// liveTitle starts the title of every issue the check creates.
const liveTitle = "agent-overflow live check "

// sweep trashes the issues an earlier run of the check left behind.
func (l *live) sweep(team Team) {
	var left []Issue
	l.step("ListIssues leftovers", func(ctx context.Context) error {
		after := ""
		for {
			p, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{team.ID}}}, after)
			if err != nil {
				return err
			}
			for _, issue := range p.Items {
				if strings.HasPrefix(issue.Title, liveTitle) {
					left = append(left, issue)
				}
			}
			if !p.HasNextPage {
				return nil
			}
			after = p.EndCursor
		}
	})
	for i := range left {
		l.cleanup(&left[i])
	}
}

// listOrder lists the team's issues in manual order and returns the
// position of each of ids.
func (l *live) listOrder(team Team, ids []string) []Issue {
	var mine []Issue
	l.step("ListIssues manual", func(ctx context.Context) error {
		after := ""
		for {
			p, err := l.c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{team.ID}}, Order: OrderManual}, after)
			if err != nil {
				return err
			}
			for _, issue := range p.Items {
				if slices.Contains(ids, issue.ID) {
					mine = append(mine, issue)
				}
			}
			if !p.HasNextPage {
				return nil
			}
			after = p.EndCursor
		}
	})
	return mine
}

func (l *live) checkList(team Team, ids []string) {
	if got := l.listOrder(team, ids); len(got) != len(ids) {
		l.t.Errorf("the list holds %d of the %d created issues", len(got), len(ids))
	}
}

func (l *live) checkPoll(team Team, since time.Time, ids []string) {
	l.step("PollIssues", func(ctx context.Context) error {
		q := PollQuery{TeamID: team.ID, IncludeArchived: true, From: PollCursor{Since: since}}
		found := 0
		var last time.Time
		for {
			p, err := l.c.PollIssues(ctx, q)
			if err != nil {
				return err
			}
			for _, issue := range p.Items {
				if issue.UpdatedAt.Before(last) {
					return fmt.Errorf("poll order: %s updated %v after one updated %v", issue.Identifier, issue.UpdatedAt, last)
				}
				last = issue.UpdatedAt
				if slices.Contains(ids, issue.ID) {
					found++
				}
			}
			l.t.Logf("poll page: %d issues, next %+v, restarted %t", len(p.Items), p.Next, p.Restarted)
			if !p.HasNextPage {
				break
			}
			q.From = p.Next
		}
		if found != len(ids) {
			return fmt.Errorf("the poll delivered %d of the %d created issues", found, len(ids))
		}
		return nil
	})
}

// checkCursorAcrossClients measures whether a poll cursor one client
// received continues the walk in another, as a restarted sync loop does
// with a stored PollCursor: it reads the walk's end cursor, updates an
// issue the walk already delivered, and continues from that cursor in a
// second client. The updated issue moves past the cursor, so the second
// client must deliver it without restarting.
func (l *live) checkCursorAcrossClients(team Team, since time.Time, issue *Issue) {
	var from PollCursor
	if !l.step("PollIssues for a cursor", func(ctx context.Context) error {
		q := PollQuery{TeamID: team.ID, IncludeArchived: true, From: PollCursor{Since: since}}
		for {
			p, err := l.c.PollIssues(ctx, q)
			if err != nil {
				return err
			}
			if !p.HasNextPage {
				if p.EndCursor == "" {
					return errors.New("the last page carries no end cursor")
				}
				from = PollCursor{Since: q.From.Since, After: p.EndCursor, Floor: p.Next.Floor}
				return nil
			}
			q.From = p.Next
		}
	}) {
		return
	}
	var updated *Issue
	if !l.step("UpdateIssue past the cursor", func(ctx context.Context) (err error) {
		updated, err = l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{Priority: Set(PriorityLow)})
		return err
	}) {
		return
	}
	other, err := New(Options{Version: "live-check", Credentials: l.creds})
	if err != nil {
		l.t.Errorf("a second client: %v", err)
		return
	}
	defer other.Close()
	l.step("PollIssues from the cursor in a second client", func(ctx context.Context) error {
		p, err := other.PollIssues(ctx, PollQuery{TeamID: team.ID, IncludeArchived: true, From: from})
		if err != nil {
			return err
		}
		l.t.Logf("continued walk: %d issues, restarted %t", len(p.Items), p.Restarted)
		if p.Restarted {
			return errors.New("Linear refused the cursor another client received")
		}
		for _, got := range p.Items {
			if got.ID == issue.ID && !got.UpdatedAt.Before(updated.UpdatedAt) {
				return nil
			}
		}
		return fmt.Errorf("the continued walk did not deliver %s, updated after the cursor", issue.Identifier)
	})
}

// checkReorder moves the third issue between the first two and reads
// the order back.
func (l *live) checkReorder(team Team, created []*Issue) {
	ids := []string{created[0].ID, created[1].ID, created[2].ID}
	order := l.listOrder(team, ids)
	if len(order) != 3 {
		return
	}
	first, second := order[0], order[1]
	moving := order[2]
	l.step("MoveIssue", func(ctx context.Context) error {
		_, err := l.c.MoveIssue(ctx, MoveInput{IssueID: moving.ID, Field: OrderSort, Before: &first.SortOrder, After: &second.SortOrder})
		return err
	})
	got := l.listOrder(team, ids)
	var keys []string
	for _, issue := range got {
		keys = append(keys, fmt.Sprintf("%s(%v)", issue.Identifier, issue.SortOrder))
	}
	l.t.Logf("manual order after the move: %v", keys)
	if len(got) != 3 || got[0].ID != first.ID || got[1].ID != moving.ID || got[2].ID != second.ID {
		l.t.Errorf("the moved issue is not between its neighbors")
	}
}

func (l *live) checkComments(issue *Issue) {
	var top, reply *Comment
	if !l.step("CreateComment", func(ctx context.Context) (err error) {
		top, err = l.c.CreateComment(ctx, CreateCommentInput{IssueID: issue.ID, Body: "Live check **comment**."})
		return err
	}) {
		return
	}
	l.step("CreateComment reply", func(ctx context.Context) (err error) {
		reply, err = l.c.CreateComment(ctx, CreateCommentInput{IssueID: issue.ID, Body: "Live check reply.", ParentID: top.ID})
		return err
	})
	l.step("UpdateComment", func(ctx context.Context) error {
		_, err := l.c.UpdateComment(ctx, top.ID, "Live check comment, edited.")
		return err
	})
	l.step("IssueComments", func(ctx context.Context) error {
		p, err := l.c.IssueComments(ctx, issue.ID, "")
		if err == nil && len(p.Items) < 2 {
			err = fmt.Errorf("%d comments read back", len(p.Items))
		}
		return err
	})
	if reply != nil {
		l.step("DeleteComment", func(ctx context.Context) error { return l.c.DeleteComment(ctx, reply.ID) })
	}
}

func (l *live) checkUpdate(team Team, viewer *Viewer, issue *Issue) {
	in := UpdateIssueInput{
		Title: Set(issue.Title + " (updated)"), Priority: Set(PriorityHigh),
		AssigneeID: Set(viewer.User.ID), Description: Null[string](),
	}
	if team.Estimation.Scale != EstimateNotUsed && team.Estimation.Scale != "" {
		in.Estimate = Set(2)
	}
	l.step("UpdateIssue", func(ctx context.Context) error {
		got, err := l.c.UpdateIssue(ctx, issue.ID, in)
		if err == nil && (got.Priority != PriorityHigh || got.AssigneeID != viewer.User.ID || got.Description != "") {
			err = fmt.Errorf("stored %+v", got)
		}
		return err
	})
	l.step("UpdateIssue clear", func(ctx context.Context) error {
		got, err := l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{AssigneeID: Null[string](), Estimate: Null[int]()})
		if err == nil && (got.AssigneeID != "" || got.Estimate != nil) {
			err = fmt.Errorf("stored assignee %q estimate %v", got.AssigneeID, got.Estimate)
		}
		return err
	})
	l.step("IssueHistory", func(ctx context.Context) error {
		p, err := l.c.IssueHistory(ctx, issue.ID, "")
		if err == nil {
			l.t.Logf("history: %d entries", len(p.Items))
		}
		return err
	})
	l.step("IssueStateSpans", func(ctx context.Context) error {
		p, err := l.c.IssueStateSpans(ctx, issue.ID, "")
		if err == nil {
			l.t.Logf("state spans: %d", len(p.Items))
		}
		return err
	})
}

func (l *live) checkLinks(created []*Issue) {
	var rel *Relation
	l.step("CreateRelation", func(ctx context.Context) (err error) {
		rel, err = l.c.CreateRelation(ctx, CreateRelationInput{IssueID: created[0].ID, RelatedIssueID: created[1].ID, Type: RelationBlocks})
		return err
	})
	l.step("LinkURL", func(ctx context.Context) error {
		_, err := l.c.LinkURL(ctx, LinkInput{IssueID: created[0].ID, URL: "https://example.com/agent-overflow-live-check/pull/1", Title: "Live check link", Subtitle: "Open"})
		return err
	})
	l.step("Issue", func(ctx context.Context) error {
		d, err := l.c.Issue(ctx, created[0].Identifier)
		if err == nil && (len(d.Attachments) != 1 || len(d.Relations) != 1 || len(d.Comments.Items) == 0) {
			err = fmt.Errorf("read back %d attachments, %d relations, %d comments", len(d.Attachments), len(d.Relations), len(d.Comments.Items))
		}
		return err
	})
	if rel != nil {
		l.step("DeleteRelation", func(ctx context.Context) error { return l.c.DeleteRelation(ctx, rel.ID) })
	}
}
