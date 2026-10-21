package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
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

	selected, err := s.ListDraftDiffReviewCommentsByID(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID}, DiffReviewMessageRows{})
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

	if _, err := s.ListDraftDiffReviewCommentsByID(ThreadReviewOwner(thread.ID), string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID, "missing"}, DiffReviewMessageRows{}); err == nil {
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

// claimFor claims the owner's comment for the message the row itemID of
// thread carries, in the turn turnID.
func claimFor(t *testing.T, s *Store, owner DiffReviewOwner, scope, source, comment, thread, itemID, turnID string, queued bool) {
	t.Helper()
	claimed, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, []DiffReviewCommentSnapshot{{ID: comment, Body: comment}}, DiffReviewDelivery{
		SentAt: 10, TurnID: turnID, ThreadID: thread, ItemID: itemID, Queued: queued,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %s = %v, %v", comment, claimed, err)
	}
}

func createReviewComments(t *testing.T, s *Store, owner DiffReviewOwner, scope, source string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.CreateDiffReviewComment(DiffReviewComment{
			ID: id, ThreadID: owner.ThreadID, PRKey: owner.PRKey, Scope: scope, SourceKey: source, FilePath: "a.go", NewLine: 1, Side: "new", Body: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func reviewCommentStatus(t *testing.T, s *Store, owner DiffReviewOwner, id string) string {
	t.Helper()
	comment, err := s.GetDiffReviewComment(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return comment.Status
}

// A queued message's claim settles when the provider confirms the message,
// not when its row lands: a confirmation that places a group settles the
// confirmed row's claim and not the claim of a sibling that moves with it
// unconfirmed, one that inserts a deferred row settles that row's, and one
// in place settles its row's. A boot returns what no confirmation settled.
func TestQueuedDiffReviewClaimsSettleWhenTheProviderConfirmsTheMessage(t *testing.T) {
	s := newTestStore(t)
	seedPlacement(t, s)
	owner := ThreadReviewOwner("t")
	scope, source := string(DiffReviewScopeWorkspace), "k"
	createReviewComments(t, s, owner, scope, source, "placed", "sibling", "in-place", "deferred")
	claimFor(t, s, owner, scope, source, "placed", "t", "a", "t:0", true)
	claimFor(t, s, owner, scope, source, "sibling", "t", "b", "t:0", true)
	claimFor(t, s, owner, scope, source, "in-place", "t", "c", "t:0", true)
	claimFor(t, s, owner, scope, source, "deferred", "t", "d", "t:0", true)
	// The quiet row of in-place lands before the provider has the message.
	if err := s.InsertItem(placementItem("t", "c", "user_text", 4)); err != nil {
		t.Fatal(err)
	}
	var settles string
	if err := s.db.QueryRow(`SELECT claim_settles FROM diff_review_comments WHERE id = 'in-place'`).Scan(&settles); err != nil || settles != diffReviewClaimSettlesAtConfirm {
		t.Fatalf("in-place claim after its row = %q (%v), want it waiting for the confirmation", settles, err)
	}

	same := func(meta string, _ int) (string, error) { return meta, nil }
	boundary, err := s.CaptureUserPlacementBoundary("t", 0, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, []Item{{ID: "a"}, {ID: "b"}}, same, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateItemMetaAtBoundary("t", "c", "", same, 21); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceUserItemsAfterBoundary("t", 0, "", []Item{placementItem("t", "d", "user_text", 0)}, same, 22); err != nil {
		t.Fatal(err)
	}

	released, err := s.ReleaseUnsettledDiffReviewClaims()
	if err != nil || released != 1 {
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want the unconfirmed sibling's claim", released, err)
	}
	for id, want := range map[string]string{"placed": "sent", "sibling": "draft", "in-place": "sent", "deferred": "sent"} {
		if got := reviewCommentStatus(t, s, owner, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

// A message the provider retired undelivered takes back the review drafts
// it claimed with its row, in one transaction: a direct send's, whose row
// settled its claim, a queued one's, and a deferred one's, which has no row.
// Another message's drafts stay sent.
func TestRetireUndeliveredUserRowReturnsTheDraftsItsMessageClaimed(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("retire-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	pr := PRReviewOwner("forge:o/r:7")
	prScope, prSource := string(DiffReviewScopePR), "pr:forge:o/r:7"
	own := ThreadReviewOwner(thread.ID)
	scope, source := string(DiffReviewScopeWorkspace), "k"
	createReviewComments(t, s, pr, prScope, prSource, "direct", "kept")
	createReviewComments(t, s, own, scope, source, "queued", "deferred")
	userRow := func(id string) {
		t.Helper()
		if _, err := s.UpsertItem(Item{ID: id, ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: id, CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	claimFor(t, s, pr, prScope, prSource, "direct", thread.ID, "user:1", thread.ID+":1", false)
	userRow("user:1")
	claimFor(t, s, own, scope, source, "queued", thread.ID, "user:2", thread.ID+":1", true)
	userRow("user:2")
	claimFor(t, s, own, scope, source, "deferred", thread.ID, "user:3", thread.ID+":1", true)
	claimFor(t, s, pr, prScope, prSource, "kept", thread.ID, "user:4", thread.ID+":1", false)
	userRow("user:4")

	// A delete that fails returns nothing.
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_delete BEFORE DELETE ON items WHEN OLD.id = 'user:1' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireUndeliveredUserRow(thread.ID, "user:1"); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("retire = %v, want the injected failure", err)
	}
	if got := reviewCommentStatus(t, s, pr, "direct"); got != "sent" {
		t.Fatalf("direct after a failed retire = %s, want sent", got)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_delete`); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		row     string
		comment string
		owner   DiffReviewOwner
		want    DiffReviewCommentSource
	}{
		{"user:1", "direct", pr, DiffReviewCommentSource{Owner: pr, Scope: prScope, SourceKey: prSource}},
		{"user:2", "queued", own, DiffReviewCommentSource{Owner: own, Scope: scope, SourceKey: source}},
		{"user:3", "deferred", own, DiffReviewCommentSource{Owner: own, Scope: scope, SourceKey: source}},
	} {
		sources, err := s.RetireUndeliveredUserRow(thread.ID, tc.row)
		want := []DiffReviewCommentSnapshot{{ID: tc.comment, Body: tc.comment}}
		if err != nil || len(sources) != 1 || sources[0].DiffReviewCommentSource != tc.want || !reflect.DeepEqual(sources[0].Comments, want) {
			t.Fatalf("retire %s = %+v, %v; want %+v with %+v", tc.row, sources, err, tc.want, want)
		}
		if _, found, err := s.GetThreadItem(thread.ID, tc.row); err != nil || found {
			t.Fatalf("row %s after its retire: found=%v err=%v", tc.row, found, err)
		}
		comment, err := s.GetDiffReviewComment(tc.owner, tc.comment)
		if err != nil || comment.Status != "draft" || comment.SentAt != 0 || comment.SentTurnID != "" {
			t.Fatalf("%s after its message's retire = %+v (%v), want a draft", tc.comment, comment, err)
		}
	}
	if sources, err := s.RetireUndeliveredUserRow(thread.ID, "user:3"); err != nil || len(sources) != 0 {
		t.Fatalf("second retire = %+v, %v; want nothing left", sources, err)
	}
	// A message carrying comments of two sources gets each source's back.
	createReviewComments(t, s, pr, prScope, prSource, "joined-pr")
	createReviewComments(t, s, own, scope, source, "joined-own-1", "joined-own-2")
	claimFor(t, s, pr, prScope, prSource, "joined-pr", thread.ID, "user:5", thread.ID+":1", true)
	claimFor(t, s, own, scope, source, "joined-own-1", thread.ID, "user:5", thread.ID+":1", true)
	claimFor(t, s, own, scope, source, "joined-own-2", thread.ID, "user:5", thread.ID+":1", true)
	joined, err := s.RetireUndeliveredUserRow(thread.ID, "user:5")
	if err != nil {
		t.Fatal(err)
	}
	got := map[DiffReviewCommentSource][]string{}
	for _, source := range joined {
		for _, comment := range source.Comments {
			got[source.DiffReviewCommentSource] = append(got[source.DiffReviewCommentSource], comment.ID+"="+comment.Body)
		}
	}
	for key := range got {
		sort.Strings(got[key])
	}
	wantJoined := map[DiffReviewCommentSource][]string{
		{Owner: pr, Scope: prScope, SourceKey: prSource}: {"joined-pr=joined-pr"},
		{Owner: own, Scope: scope, SourceKey: source}:    {"joined-own-1=joined-own-1", "joined-own-2=joined-own-2"},
	}
	if len(joined) != 2 || !reflect.DeepEqual(got, wantJoined) {
		t.Fatalf("retire of a two-source message = %+v, want %+v", joined, wantJoined)
	}
	if got := reviewCommentStatus(t, s, pr, "kept"); got != "sent" {
		t.Fatalf("kept = %s, want sent", got)
	}
	if released, err := s.ReleaseUnsettledDiffReviewClaims(); err != nil || released != 0 {
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want nothing pending", released, err)
	}
}

// An edit, a publish and a resolve each supersede the message that claimed
// a comment, on a thread's comments and a PR's: none leaves a claim that a
// boot or the message's undelivered retire could turn back into a draft.
func TestSupersedingAClaimedCommentClearsItsClaim(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("supersede-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	owners := []DiffReviewOwner{ThreadReviewOwner(thread.ID), PRReviewOwner("forge:o/r:7")}
	sourceOf := func(owner DiffReviewOwner) (string, string) {
		if owner.PRKey != "" {
			return string(DiffReviewScopePR), "pr:forge:o/r:7"
		}
		return string(DiffReviewScopeWorkspace), "k"
	}
	claimed := func(id string) bool {
		t.Helper()
		var linked bool
		if err := s.db.QueryRow(`SELECT claim_item_id <> '' OR claim_thread_id <> '' OR claim_settles <> '' FROM diff_review_comments WHERE id = ?`, id).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		return linked
	}
	for i, owner := range owners {
		scope, source := sourceOf(owner)
		published, resolved := fmt.Sprintf("published-%d", i), fmt.Sprintf("resolved-%d", i)
		createReviewComments(t, s, owner, scope, source, published, resolved)
		claimFor(t, s, owner, scope, source, published, thread.ID, "user:1", thread.ID+":1", true)
		claimFor(t, s, owner, scope, source, resolved, thread.ID, "user:1", thread.ID+":1", false)
		if _, err := s.UpdateDiffReviewComment(owner, published, DiffReviewCommentUpdate{Body: "edited"}, 20); err != nil {
			t.Fatal(err)
		}
		if claimed(published) {
			t.Fatalf("%s: the edit kept the claim", owner)
		}
		if err := s.MarkDiffReviewCommentsSent(owner, scope, source, []string{published}, 30, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteOrResolveDiffReviewComment(owner, resolved, 40); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{published, resolved} {
			if claimed(id) {
				t.Fatalf("%s: %s keeps its claim", owner, id)
			}
		}
	}
	if sources, err := s.RetireUndeliveredUserRow(thread.ID, "user:1"); err != nil || len(sources) != 0 {
		t.Fatalf("retire = %+v, %v; want nothing the message still carries", sources, err)
	}
	if released, err := s.ReleaseUnsettledDiffReviewClaims(); err != nil || released != 0 {
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want nothing", released, err)
	}
	for i, owner := range owners {
		comment, err := s.GetDiffReviewComment(owner, fmt.Sprintf("published-%d", i))
		if err != nil || comment.Status != "sent" || comment.Body != "edited" || comment.SentTurnID != "" {
			t.Fatalf("%s published = %+v (%v), want the edit sent with no turn", owner, comment, err)
		}
		if got := reviewCommentStatus(t, s, owner, fmt.Sprintf("resolved-%d", i)); got != "resolved" {
			t.Fatalf("%s resolved = %s", owner, got)
		}
	}
}
