package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDiffReviewCommentsDraftLifecycle(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	created, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:           "comment-1",
		ThreadID:     thread.ID,
		Scope:        string(DiffReviewScopeWorkspace),
		SourceKey:    "diff-1",
		FilePath:     "app.ts",
		NewLine:      12,
		Side:         "new",
		SelectedText: "+next",
		Body:         "Use a clearer name.",
		CreatedAt:    100,
		UpdatedAt:    100,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}
	if created.Status != "draft" || created.NewLine != 12 || created.Side != "new" {
		t.Fatalf("created = %+v", created)
	}

	updated, err := s.UpdateDiffReviewComment(ThreadReviewOwner(thread.ID), created.ID, DiffReviewCommentUpdate{
		Body: "Use the domain term.",
	}, 200)
	if err != nil {
		t.Fatalf("UpdateDiffReviewComment: %v", err)
	}
	if updated.Body != "Use the domain term." || updated.UpdatedAt != 200 {
		t.Fatalf("updated = %+v", updated)
	}

	selected, err := s.ListDraftDiffReviewCommentsByID(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID})
	if err != nil {
		t.Fatalf("ListDraftDiffReviewCommentsByID: %v", err)
	}
	if len(selected) != 1 || selected[0].ID != created.ID {
		t.Fatalf("selected = %+v", selected)
	}

	if err := s.MarkDiffReviewCommentsSent(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID}, 300, "user:1"); err != nil {
		t.Fatalf("MarkDiffReviewCommentsSent: %v", err)
	}
	if err := s.MarkDiffReviewCommentsSent(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID}, 300, "user:1"); err != nil {
		t.Fatalf("MarkDiffReviewCommentsSent idempotent retry: %v", err)
	}
	sent, err := s.GetDiffReviewComment(ThreadReviewOwner(thread.ID), created.ID)
	if err != nil {
		t.Fatalf("GetDiffReviewComment: %v", err)
	}
	if sent.Status != "sent" || sent.SentAt != 300 || sent.SentTurnID != "user:1" {
		t.Fatalf("sent = %+v", sent)
	}

	if err := s.DeleteOrResolveDiffReviewComment(ThreadReviewOwner(thread.ID), created.ID, 400); err != nil {
		t.Fatalf("DeleteOrResolveDiffReviewComment: %v", err)
	}
	resolved, err := s.GetDiffReviewComment(ThreadReviewOwner(thread.ID), created.ID)
	if err != nil {
		t.Fatalf("GetDiffReviewComment resolved: %v", err)
	}
	if resolved.Status != "resolved" || resolved.UpdatedAt != 400 {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestDiffReviewCommentsRejectInvalidScopeAndAnchors(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-invalid-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if _, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "bad-scope",
		ThreadID:  thread.ID,
		Scope:     "junk",
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		Side:      "file",
		Body:      "Nope.",
	}); err == nil {
		t.Fatal("CreateDiffReviewComment accepted invalid scope")
	}

	if _, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "bad-file-anchor",
		ThreadID:  thread.ID,
		Scope:     string(DiffReviewScopeWorkspace),
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		NewLine:   1,
		Side:      "file",
		Body:      "Nope.",
	}); err == nil {
		t.Fatal("CreateDiffReviewComment accepted file comment with line number")
	}
}

func TestNormalizeDiffReviewScopeAcceptsLocalScopes(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"turn", string(DiffReviewScopeTurn)},
		{"session", string(DiffReviewScopeSession)},
		{"workspace", string(DiffReviewScopeWorkspace)},
		{"branch", string(DiffReviewScopeBranch)},
		{"pr", string(DiffReviewScopePR)},
		{" branch ", string(DiffReviewScopeBranch)},
	}
	for _, tt := range tests {
		got, err := NormalizeDiffReviewScope(tt.in)
		if err != nil {
			t.Fatalf("NormalizeDiffReviewScope(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("NormalizeDiffReviewScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	if _, err := NormalizeDiffReviewScope("junk"); err == nil {
		t.Fatal("NormalizeDiffReviewScope accepted junk")
	}
}

func TestDiffReviewCommentsAcceptAllLocalScopes(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-all-scopes-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	for _, scope := range []DiffReviewScope{
		DiffReviewScopeTurn,
		DiffReviewScopeSession,
		DiffReviewScopeWorkspace,
		DiffReviewScopeBranch,
		DiffReviewScopePR,
	} {
		t.Run(string(scope), func(t *testing.T) {
			created, err := s.CreateDiffReviewComment(DiffReviewComment{
				ID:        "comment-" + string(scope),
				ThreadID:  thread.ID,
				Scope:     string(scope),
				SourceKey: "diff-" + string(scope),
				FilePath:  "app.ts",
				NewLine:   1,
				Side:      "new",
				Body:      "Comment for " + string(scope),
				CreatedAt: 100,
				UpdatedAt: 100,
			})
			if err != nil {
				t.Fatalf("CreateDiffReviewComment(%s): %v", scope, err)
			}
			if created.Scope != string(scope) {
				t.Fatalf("created.Scope = %q, want %q", created.Scope, scope)
			}
		})
	}
}

func TestDiffReviewCommentCommitSHARoundTrip(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-pr-sha-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	created, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "comment-pr",
		ThreadID:  thread.ID,
		Scope:     string(DiffReviewScopePR),
		SourceKey: "pr:github:o/r:1",
		CommitSHA: "abc123",
		FilePath:  "app.ts",
		NewLine:   1,
		Side:      "new",
		Body:      "PR comment",
		CreatedAt: 100,
		UpdatedAt: 100,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}
	if created.CommitSHA != "abc123" {
		t.Fatalf("CommitSHA = %q, want abc123", created.CommitSHA)
	}
	listed, err := s.ListDiffReviewComments(ThreadReviewOwner(thread.ID), string(DiffReviewScopePR), "pr:github:o/r:1")
	if err != nil {
		t.Fatalf("ListDiffReviewComments: %v", err)
	}
	if len(listed) != 1 || listed[0].CommitSHA != "abc123" {
		t.Fatalf("listed = %+v", listed)
	}
}

func TestListDraftDiffReviewCommentsByIDRejectsStaleIDs(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-stale-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	created, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "comment-1",
		ThreadID:  thread.ID,
		Scope:     string(DiffReviewScopeWorkspace),
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		NewLine:   12,
		Side:      "new",
		Body:      "Use a clearer name.",
		CreatedAt: 100,
		UpdatedAt: 100,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}

	if _, err := s.ListDraftDiffReviewCommentsByID(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID, "missing"}); err == nil {
		t.Fatal("ListDraftDiffReviewCommentsByID accepted a stale id")
	}
}

// A claim settles exactly the text the message carries: an edit since
// stays a draft, another turn's claim refuses the whole claim and marks
// nothing, the same turn's is left as it is, and a release or retarget
// moves only what the claim marked.
func TestClaimDiffReviewCommentsSentSettlesTheTextTheMessageCarries(t *testing.T) {
	s := newTestStore(t)
	owner := PRReviewOwner("github:o/r:7")
	scope, source := string(DiffReviewScopePR), "pr:github:o/r:7"
	create := func(id, body string) {
		t.Helper()
		if _, err := s.CreateDiffReviewComment(DiffReviewComment{
			ID: id, PRKey: owner.PRKey, Scope: scope, SourceKey: source, FilePath: "a.go", NewLine: 1, Side: "new", Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	status := func(id string) (string, string) {
		t.Helper()
		comment, err := s.GetDiffReviewComment(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		return comment.Status, comment.SentTurnID
	}
	create("a", "first")
	create("b", "second")
	create("c", "third")
	snapshot := []DiffReviewCommentSnapshot{{ID: "a", Body: "first"}, {ID: "b", Body: "second"}, {ID: "gone", Body: "x"}}
	if _, err := s.UpdateDiffReviewComment(owner, "b", DiffReviewCommentUpdate{Body: "second, edited"}, 50); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, snapshot, DiffReviewDelivery{SentAt: 100, TurnID: "x:1", ThreadID: "x", ItemID: "user:1"})
	if err != nil || len(claimed) != 1 || claimed[0] != "a" {
		t.Fatalf("claim = %v, %v; want [a]", claimed, err)
	}
	if st, turn := status("a"); st != "sent" || turn != "x:1" {
		t.Fatalf("a = %s %s", st, turn)
	}
	if st, _ := status("b"); st != "draft" {
		t.Fatalf("the edited b = %s, want it left a draft", st)
	}
	// The same turn again (a joined message naming a twice) is no conflict.
	if again, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, snapshot[:1], DiffReviewDelivery{SentAt: 100, TurnID: "x:1", ThreadID: "x", ItemID: "user:1"}); err != nil || len(again) != 0 {
		t.Fatalf("same turn = %v, %v", again, err)
	}
	// Another turn naming c and a: refused whole, c untouched.
	other := []DiffReviewCommentSnapshot{{ID: "c", Body: "third"}, {ID: "a", Body: "first"}}
	if got, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, other, DiffReviewDelivery{SentAt: 200, TurnID: "y:4", ThreadID: "y", ItemID: "user:4"}); !errors.Is(err, ErrDiffReviewCommentsSentElsewhere) || got != nil {
		t.Fatalf("another turn = %v, %v; want ErrDiffReviewCommentsSentElsewhere", got, err)
	}
	if st, _ := status("c"); st != "draft" {
		t.Fatalf("c = %s after a refused claim", st)
	}

	if err := s.RetargetDiffReviewCommentsSent(owner, scope, source, []string{"a", "b"}, "x:1", "x:2", "user:2"); err != nil {
		t.Fatal(err)
	}
	if st, turn := status("a"); st != "sent" || turn != "x:2" {
		t.Fatalf("retargeted a = %s %s", st, turn)
	}
	if st, turn := status("b"); st != "draft" || turn != "" {
		t.Fatalf("b moved with the retarget: %s %s", st, turn)
	}
	if err := s.ReleaseDiffReviewCommentsSent(owner, scope, source, []string{"a"}, "x:1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := status("a"); st != "sent" {
		t.Fatal("a release naming a turn a is no longer in moved it")
	}
	if err := s.ReleaseDiffReviewCommentsSent(owner, scope, source, []string{"a"}, "x:2"); err != nil {
		t.Fatal(err)
	}
	if comment, err := s.GetDiffReviewComment(owner, "a"); err != nil || comment.Status != "draft" || comment.SentAt != 0 || comment.SentTurnID != "" {
		t.Fatalf("released a = %+v, %v", comment, err)
	}
}

// A claim is settled by the insert of the row that carries it, in that
// row's transaction. A claim whose row never landed (the app stopped
// between the two) returns to a draft at the next boot; a settled one,
// one retargeted to the row that landed, and one whose row was already
// there stay sent.
func TestDiffReviewClaimsTheirRowNeverSettledReturnAtBoot(t *testing.T) {
	s := openStoreAt(t)
	thread := makeThread("claim-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	owner := PRReviewOwner("github:o/r:7")
	scope, source := string(DiffReviewScopePR), "pr:github:o/r:7"
	for _, id := range []string{"landed", "lost", "moved", "present"} {
		if _, err := s.CreateDiffReviewComment(DiffReviewComment{
			ID: id, PRKey: owner.PRKey, Scope: scope, SourceKey: source, FilePath: "a.go", NewLine: 1, Side: "new", Body: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	userRow := func(id string, turn int) error {
		_, err := s.UpsertItem(Item{ID: id, ThreadID: thread.ID, TurnIndex: turn, Kind: "user_text", Role: "user", Status: "completed", Summary: id, CreatedAt: 1, UpdatedAt: 1}, nil)
		return err
	}
	claim := func(comment, itemID string, turn int) {
		t.Helper()
		claimed, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, []DiffReviewCommentSnapshot{{ID: comment, Body: comment}}, DiffReviewDelivery{
			SentAt: 10, TurnID: fmt.Sprintf("%s:%d", thread.ID, turn), ThreadID: thread.ID, ItemID: itemID,
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim %s = %v, %v", comment, claimed, err)
		}
	}

	claim("landed", "user:1", 1)
	if err := userRow("user:1", 1); err != nil {
		t.Fatal(err)
	}

	claim("lost", "user:2", 2)
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_user_row BEFORE INSERT ON items WHEN NEW.id = 'user:2' BEGIN SELECT RAISE(ABORT, 'injected persist failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := userRow("user:2", 2); err == nil || !strings.Contains(err.Error(), "injected persist failure") {
		t.Fatalf("user:2 insert = %v, want the injected failure", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_user_row`); err != nil {
		t.Fatal(err)
	}

	claim("moved", "user:3", 3)
	if err := s.RetargetDiffReviewCommentsSent(owner, scope, source, []string{"moved"}, thread.ID+":3", thread.ID+":4", "user:4"); err != nil {
		t.Fatal(err)
	}
	if err := userRow("user:4", 4); err != nil {
		t.Fatal(err)
	}

	if err := userRow("user:5", 5); err != nil {
		t.Fatal(err)
	}
	claim("present", "user:5", 5)

	// The process stops here: nothing released the claim user:2 made.
	s = reopenStore(t, s)
	released, err := s.ReleaseUnsettledDiffReviewClaims()
	if err != nil || released != 1 {
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want the one claim no row settled", released, err)
	}
	for id, want := range map[string]string{"landed": thread.ID + ":1", "lost": "", "moved": thread.ID + ":4", "present": thread.ID + ":5"} {
		comment, err := s.GetDiffReviewComment(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "sent"
		if want == "" {
			wantStatus = "draft"
		}
		if comment.Status != wantStatus || comment.SentTurnID != want || (want == "") != (comment.SentAt == 0) {
			t.Errorf("%s = %s in %q at %d, want %s in %q", id, comment.Status, comment.SentTurnID, comment.SentAt, wantStatus, want)
		}
	}
	if again, err := s.ReleaseUnsettledDiffReviewClaims(); err != nil || again != 0 {
		t.Fatalf("second release = %d, %v; want nothing left", again, err)
	}
	// The released draft can be claimed again.
	claim("lost", "user:6", 6)
}
