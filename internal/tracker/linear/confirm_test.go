package linear

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// A payload that says success: true proves the write only when its entity
// names it: a created entity has an ID, a write by ID answers with that
// entity, and a delete names the entity it deleted. Anything else is an
// unknown outcome that reports nothing stored.
func TestWriteAnswerNamesTheWrite(t *testing.T) {
	t.Parallel()
	issue := func(id, identifier string) string {
		return lineartest.IssueJSON(id, identifier, "2026-11-02T10:00:00.000Z", 1)
	}
	noID := strings.Replace(issue("issue-1", "WEB-1"), `"id":"issue-1"`, `"id":""`, 1)
	moved := strings.TrimSuffix(issue("issue-1", "APP-3"), "}") + `,"previousIdentifiers":["WEB-1"]}`
	comment := func(id string) string { c := lineartest.NewComment(); c.ID = id; return c.JSON() }
	cycle := func(id string) string {
		return lineartest.Cycle{ID: id, Number: 2, Team: "team-web", StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z"}.JSON()
	}
	createVars := `{"input":{"teamId":"team-web","title":"T"}}`
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ex   lineartest.Exchange
		call func(c *Client) error
		want outcome
	}{
		{"create with no ID", lineartest.Exchange{Op: "CreateIssue", Vars: createVars, Body: lineartest.Result(noID)},
			func(c *Client) error {
				_, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "team-web", Title: "T"})
				return err
			}, unknown},
		{"update answered by another issue", lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"title":"T"}}`, Body: lineartest.Result(issue("issue-2", "WEB-2"))},
			func(c *Client) error {
				_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{Title: Set("T")})
				return err
			}, unknown},
		{"update by ID", lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"issue-1","input":{"title":"T"}}`, Body: lineartest.Result(issue("issue-1", "WEB-1"))},
			func(c *Client) error {
				_, err := c.UpdateIssue(ctx, "issue-1", UpdateIssueInput{Title: Set("T")})
				return err
			}, applied},
		{"update by identifier in another case", lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"web-1","input":{"title":"T"}}`, Body: lineartest.Result(issue("issue-1", "WEB-1"))},
			func(c *Client) error {
				_, err := c.UpdateIssue(ctx, "web-1", UpdateIssueInput{Title: Set("T")})
				return err
			}, applied},
		{"team move by the old identifier", lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"teamId":"team-app"}}`, Body: lineartest.Result(moved)},
			func(c *Client) error {
				_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{TeamID: Set("team-app")})
				return err
			}, applied},
		{"archive answered by another issue", lineartest.Exchange{Op: "ArchiveIssue", Vars: `{"id":"WEB-1"}`, Body: lineartest.Result(issue("issue-2", "WEB-2"))},
			func(c *Client) error { _, err := c.ArchiveIssue(ctx, "WEB-1"); return err }, unknown},
		{"comment update answered by another comment", lineartest.Exchange{Op: "UpdateComment", Vars: `{"id":"comment-2","input":{"body":"x"}}`, Body: lineartest.Result(comment("comment-3"))},
			func(c *Client) error { _, err := c.UpdateComment(ctx, "comment-2", "x"); return err }, unknown},
		{"comment created with no ID", lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"WEB-1","body":"x"}}`, Body: lineartest.Result(comment(""))},
			func(c *Client) error {
				_, err := c.CreateComment(ctx, CreateCommentInput{IssueID: "WEB-1", Body: "x"})
				return err
			}, unknown},
		{"cycle start answered by another cycle", lineartest.Exchange{Op: "StartUpcomingCycle", Vars: `{"id":"cycle-2"}`, Body: lineartest.Result(cycle("cycle-3"))},
			func(c *Client) error { _, err := c.StartUpcomingCycle(ctx, "cycle-2"); return err }, unknown},
		{"comment delete naming another comment", lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-2"}`, Body: lineartest.Result(`"comment-3"`)},
			func(c *Client) error { return c.DeleteComment(ctx, "comment-2") }, unknown},
		{"comment delete naming none", lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-2"}`, Body: `{"data":{"result":{"success":true}}}`},
			func(c *Client) error { return c.DeleteComment(ctx, "comment-2") }, unknown},
		{"relation delete naming it", lineartest.Exchange{Op: "DeleteRelation", Vars: `{"id":"rel-1"}`, Body: lineartest.Result(`"rel-1"`)},
			func(c *Client) error { return c.DeleteRelation(ctx, "rel-1") }, applied},
	} {
		err := tc.call(newFake(t, tc.ex).client(t))
		if got := outcomeOf(err); got != tc.want {
			t.Errorf("%s: %s, want %s (%v)", tc.name, got, tc.want, err)
		}
	}
}
