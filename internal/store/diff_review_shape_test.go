package store

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A range draft keeps its first line; a range must start before it ends
// on a half of the diff both lines are on, and only a line has one.
func TestDiffReviewRangeDrafts(t *testing.T) {
	s := newTestStore(t)
	owner := PRReviewOwner("github:o/r:1")
	base := DiffReviewComment{PRKey: owner.PRKey, Scope: "pr", SourceKey: "pr:github:o/r:1", FilePath: "a.go", Body: "b"}
	create := func(id string, edit func(*DiffReviewComment)) (DiffReviewComment, error) {
		comment := base
		comment.ID = id
		edit(&comment)
		return s.CreateDiffReviewComment(comment)
	}
	ok := []func(*DiffReviewComment){
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartNewLine = "new", 18, 12 },
		func(c *DiffReviewComment) { c.Side, c.OldLine, c.StartOldLine = "old", 9, 4 },
		// From an unchanged line to an added one, and from a removed line to
		// an unchanged one: each pair shares a half.
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartOldLine, c.StartNewLine = "new", 18, 10, 11 },
		func(c *DiffReviewComment) { c.Side, c.OldLine, c.NewLine, c.StartOldLine = "context", 9, 10, 7 },
	}
	for i, edit := range ok {
		created, err := create("ok"+string(rune('a'+i)), edit)
		if err != nil {
			t.Fatalf("range %d: %v", i, err)
		}
		want := base
		edit(&want)
		if created.StartOldLine != want.StartOldLine || created.StartNewLine != want.StartNewLine || created.Side != want.Side {
			t.Fatalf("range %d = %+v", i, created)
		}
	}
	bad := []func(*DiffReviewComment){
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartNewLine = "new", 12, 12 },
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartNewLine = "new", 12, 18 },
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartOldLine = "new", 12, 4 },
		func(c *DiffReviewComment) { c.Side, c.StartNewLine = "file", 4 },
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.StartNewLine = "new", 12, -1 },
		func(c *DiffReviewComment) { c.Side, c.NewLine, c.ReplyRootID = "new", 12, 4 },
	}
	for i, edit := range bad {
		if created, err := create("bad"+string(rune('a'+i)), edit); err == nil {
			t.Fatalf("bad range %d accepted: %+v", i, created)
		}
	}
	listed, err := s.ListDiffReviewComments(owner, "pr", base.SourceKey)
	if err != nil || len(listed) != len(ok) {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
}

// A held reply names its thread, has no line, is pr scope, and is the
// thread's only draft reply for one owner and source; once sent, another
// may be written.
func TestDiffReviewHeldReplyDrafts(t *testing.T) {
	s := newTestStore(t)
	owner := PRReviewOwner("github:o/r:1")
	reply := func(id, source string) DiffReviewComment {
		return DiffReviewComment{ID: id, PRKey: owner.PRKey, Scope: "pr", SourceKey: source, FilePath: "a.go", ReplyThreadID: " PRRT_1 ", ReplyRootID: 7, Body: "agreed"}
	}
	created, err := s.CreateDiffReviewComment(reply("r1", "pr:x"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Side != "reply" || created.ReplyThreadID != "PRRT_1" || created.ReplyRootID != 7 || created.FilePath != "a.go" {
		t.Fatalf("reply = %+v", created)
	}
	if _, err := s.CreateDiffReviewComment(reply("r2", "pr:x")); !errors.Is(err, ErrDiffReviewReplyExists) {
		t.Fatalf("second draft reply = %v", err)
	}
	if _, err := s.CreateDiffReviewComment(reply("r3", "pr:y")); err != nil {
		t.Fatalf("a reply under another source: %v", err)
	}
	conversation := reply("r4", "pr:z")
	conversation.FilePath = ""
	if _, err := s.CreateDiffReviewComment(conversation); err != nil {
		t.Fatalf("a reply to a conversation thread: %v", err)
	}
	for i, edit := range []func(*DiffReviewComment){
		func(c *DiffReviewComment) { c.NewLine = 3 },
		func(c *DiffReviewComment) { c.Side = "file" },
		func(c *DiffReviewComment) { c.ReplyThreadID = ""; c.Side = "reply" },
		func(c *DiffReviewComment) { c.ReplyThreadID = "a\nb" },
	} {
		comment := reply("bad"+string(rune('a'+i)), "pr:w")
		edit(&comment)
		if _, err := s.CreateDiffReviewComment(comment); err == nil {
			t.Fatalf("bad reply %d accepted", i)
		}
	}
	thread := makeThread("reply-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	local := reply("r5", "k")
	local.PRKey, local.ThreadID, local.Scope = "", thread.ID, "workspace"
	if _, err := s.CreateDiffReviewComment(local); err == nil {
		t.Fatal("a reply outside pr scope was accepted")
	}
	marked, err := s.MarkPostedDiffReviewDraftsSent(owner, "pr", "pr:x", []PostedDiffReviewDraft{PostedDiffReviewDraftOf(created)}, 10, "")
	if err != nil || len(marked) != 1 {
		t.Fatalf("mark reply sent = %v, %v", marked, err)
	}
	if _, err := s.CreateDiffReviewComment(reply("r6", "pr:x")); err != nil {
		t.Fatalf("a new reply after the first was sent: %v", err)
	}
	// A posted draft whose thread differs from what was posted stays a
	// draft.
	posted := PostedDiffReviewDraftOf(created)
	posted.ID, posted.ReplyThreadID = "r6", "PRRT_other"
	if marked, err := s.MarkPostedDiffReviewDraftsSent(owner, "pr", "pr:x", []PostedDiffReviewDraft{posted}, 11, ""); err != nil || len(marked) != 0 {
		t.Fatalf("mark a reply to another thread = %v, %v", marked, err)
	}
}

// The PR context a send carries is checked field by field: a one-line
// field with a line break, a bound exceeded or a line that is not one is
// refused.
func TestDiffReviewPRContextValidate(t *testing.T) {
	good := func() *DiffReviewPRContext {
		return &DiffReviewPRContext{Number: 3, URL: "https://x/pr/3", Comments: []DiffReviewPRContextEntry{{CommentID: "r1", Thread: &DiffReviewThreadContext{
			Path: "a.go", Line: 4, StartLine: 2, Comments: []DiffReviewThreadComment{{Author: "alice", Body: "multi\nline is fine"}},
		}}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (*DiffReviewPRContext)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*DiffReviewPRContext){
		"url line break":    func(c *DiffReviewPRContext) { c.URL = "https://x\ncomment: y" },
		"path line break":   func(c *DiffReviewPRContext) { c.Comments[0].Thread.Path = "a.go\ncomment: y" },
		"author line break": func(c *DiffReviewPRContext) { c.Comments[0].Thread.Comments[0].Author = "a\rcomment: y" },
		"long author": func(c *DiffReviewPRContext) {
			c.Comments[0].Thread.Comments[0].Author = strings.Repeat("a", MaxDiffReviewThreadAuthorBytes+1)
		},
		"long body": func(c *DiffReviewPRContext) {
			c.Comments[0].Thread.Comments[0].Body = strings.Repeat("a", MaxDiffReviewCommentBodyBytes+1)
		},
		"negative line":  func(c *DiffReviewPRContext) { c.Comments[0].Thread.Line = -1 },
		"inverted range": func(c *DiffReviewPRContext) { c.Comments[0].Thread.StartLine = 9 },
		"many comments": func(c *DiffReviewPRContext) {
			c.Comments[0].Thread.Comments = make([]DiffReviewThreadComment, MaxDiffReviewThreadComments+1)
		},
	} {
		c := good()
		edit(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// One held reply per thread is the create call's rule, not the table's:
// every recovery transition that returns a sent reply to draft keeps it
// beside a newer draft reply to the same thread, and both are listed.
func TestHeldRepliesReturnToDraftBesideANewerReply(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("reply-recovery", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	owner := ThreadReviewOwner(thread.ID)
	scope, source := string(DiffReviewScopePR), "pr:github:o/r:1"
	reply := func(id string) {
		t.Helper()
		if _, err := s.CreateDiffReviewComment(DiffReviewComment{ID: id, ThreadID: thread.ID, Scope: scope, SourceKey: source, ReplyThreadID: "PRRT_1", ReplyRootID: 7, Body: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	drafts := func() []string {
		t.Helper()
		listed, err := s.ListDiffReviewComments(owner, scope, source)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range listed {
			if c.Status == "draft" {
				out = append(out, c.Body)
			}
		}
		slices.Sort(out)
		return out
	}
	userRow := func(id string) {
		t.Helper()
		if _, err := s.UpsertItem(Item{ID: id, ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: id, CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// A message the provider never consumed returns its reply.
	reply("retired")
	claimFor(t, s, owner, scope, source, "retired", thread.ID, "user:1", thread.ID+":1", true)
	userRow("user:1")
	reply("newer")
	if _, err := s.CreateDiffReviewComment(DiffReviewComment{ID: "third", ThreadID: thread.ID, Scope: scope, SourceKey: source, ReplyThreadID: "PRRT_1", Body: "third"}); !errors.Is(err, ErrDiffReviewReplyExists) {
		t.Fatalf("a second draft reply = %v", err)
	}
	if _, err := s.RetireUndeliveredUserRow(thread.ID, "user:1"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := drafts(); !slices.Equal(got, []string{"newer", "retired"}) {
		t.Fatalf("drafts after the retire = %v", got)
	}

	// A claim no row settled returns at boot.
	claimFor(t, s, owner, scope, source, "retired", thread.ID, "user:2", thread.ID+":1", false)
	claimFor(t, s, owner, scope, source, "newer", thread.ID, "user:3", thread.ID+":1", false)
	userRow("user:3")
	reply("newest")
	if released, err := s.ReleaseUnsettledDiffReviewClaims(); err != nil || released != 1 {
		t.Fatalf("release = %d, %v", released, err)
	}
	if got := drafts(); !slices.Equal(got, []string{"newest", "retired"}) {
		t.Fatalf("drafts after the release = %v", got)
	}

	// A transfer carries an unsettled claim's reply as a draft beside the
	// draft reply written since.
	claimFor(t, s, owner, scope, source, "retired", thread.ID, "user:4", thread.ID+":1", false)
	var snapshot bytes.Buffer
	if err := s.ExportThreadHistory(context.Background(), thread.ID, &snapshot); err != nil {
		t.Fatal(err)
	}
	destination := newTestStore(t)
	if err := destination.ImportThreadHistory(context.Background(), thread, bytes.NewReader(snapshot.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	listed, err := destination.ListDiffReviewComments(owner, scope, source)
	if err != nil {
		t.Fatal(err)
	}
	var carried []string
	for _, c := range listed {
		if c.Status == "draft" {
			carried = append(carried, c.Body)
		}
	}
	slices.Sort(carried)
	if !slices.Equal(carried, []string{"newest", "retired"}) {
		t.Fatalf("drafts after the transfer = %v", carried)
	}
}
