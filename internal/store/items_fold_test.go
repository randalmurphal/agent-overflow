package store

import (
	"strings"
	"testing"
	"time"
)

func seedFoldRows(t *testing.T, s *Store, threadID string, now int64) {
	t.Helper()
	createDeleteConversationThread(t, s, threadID, now)
	if err := s.InsertTurn(Turn{
		TurnID: threadID + "-turn-0", ThreadID: threadID, TurnIndex: 0, StartedAt: now,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	for i, text := range []string{"first", "second"} {
		id := "user:0:flush:" + string(rune('1'+i))
		if _, err := s.AppendItem(Item{
			ID: id, ThreadID: threadID, TurnIndex: 0,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: text, Meta: `{"sendId":"send-` + text + `"}`,
			CreatedAt: now + int64(i), UpdatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		if err := s.UpsertMessageAnchor(MessageAnchor{
			ThreadID: threadID, UserItemID: id, TurnIndex: 0,
			ProviderUserMessageID: "uuid-" + text, CreatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("anchor %s: %v", id, err)
		}
	}
}

func TestFoldUserTextRowsRewritesSurvivorAndDropsFoldedRows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	seedFoldRows(t, s, "t-fold", now)

	survivor, err := s.FoldUserTextRows(
		"t-fold", "user:0:flush:2", []string{"user:0:flush:1"}, nil,
		"first\n\n---\n\nsecond", `{"joinedSendIds":["send-first","send-second"]}`, now+10,
	)
	if err != nil {
		t.Fatalf("FoldUserTextRows: %v", err)
	}
	if survivor.Summary != "first\n\n---\n\nsecond" {
		t.Errorf("survivor summary: got %q", survivor.Summary)
	}
	if survivor.UpdatedAt != now+10 {
		t.Errorf("survivor updatedAt: got %d, want %d", survivor.UpdatedAt, now+10)
	}
	if !strings.Contains(survivor.Meta, "joinedSendIds") {
		t.Errorf("survivor meta: got %q", survivor.Meta)
	}

	items, err := s.ListItems("t-fold")
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0:flush:2" {
		t.Fatalf("remaining items: %+v, want only the survivor", items)
	}
	// Anchors follow their items through the FK cascade, so the folded row
	// cannot leave a slice anchor pointing at a message that is gone.
	anchors, err := s.ListMessageAnchors("t-fold")
	if err != nil {
		t.Fatalf("ListMessageAnchors: %v", err)
	}
	if len(anchors) != 1 || anchors[0].UserItemID != "user:0:flush:2" {
		t.Fatalf("anchors: %+v, want only the survivor's", anchors)
	}
}

func TestFoldUserTextRowsRejectsUnusableArguments(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	seedFoldRows(t, s, "t-fold-bad", now)

	cases := []struct {
		name      string
		survivor  string
		folded    []string
		wantError string
	}{
		{"no survivor", "", []string{"user:0:flush:1"}, "required"},
		{"no folded rows", "user:0:flush:2", nil, "no rows to fold"},
		{"survivor folded into itself", "user:0:flush:2", []string{"user:0:flush:2"}, "cannot also be folded"},
		{"unknown folded row", "user:0:flush:2", []string{"user:0:flush:9"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.FoldUserTextRows("t-fold-bad", tc.survivor, tc.folded, nil, "joined", "", now)
			if err == nil {
				t.Fatal("fold succeeded; want a refusal")
			}
			if tc.wantError != "" && !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error %q does not mention %q", err, tc.wantError)
			}
			// Every refusal rolls back whole: the survivor keeps its own text.
			item, found, err := s.GetThreadItem("t-fold-bad", "user:0:flush:2")
			if err != nil || !found {
				t.Fatalf("survivor missing after a refused fold (found=%v err=%v)", found, err)
			}
			if item.Summary != "second" {
				t.Errorf("survivor summary after a refused fold: got %q, want %q", item.Summary, "second")
			}
		})
	}
}

// A merged message that never had a row settles the review drafts it
// claimed with the fold, which hands its text to the survivor; a claim
// naming any other message stays pending for the boot to release.
func TestFoldUserTextRowsSettlesTheClaimsOfDeferredMembers(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	seedFoldRows(t, s, "t-fold-claims", now)
	owner := PRReviewOwner("github:o/r:7")
	scope, source := string(DiffReviewScopePR), "pr:github:o/r:7"
	for id, itemID := range map[string]string{"merged": "user:0:flush:0", "other": "user:0:flush:7"} {
		if _, err := s.CreateDiffReviewComment(DiffReviewComment{
			ID: id, PRKey: owner.PRKey, Scope: scope, SourceKey: source, FilePath: "a.go", NewLine: 1, Side: "new", Body: id,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimDiffReviewCommentsSent(owner, scope, source, []DiffReviewCommentSnapshot{{ID: id, Body: id}}, DiffReviewDelivery{
			SentAt: now, TurnID: "t-fold-claims:0", ThreadID: "t-fold-claims", ItemID: itemID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Only a deferred member: the survivor widens and nothing is deleted.
	if _, err := s.FoldUserTextRows("t-fold-claims", "user:0:flush:2", nil, []string{"user:0:flush:0"}, "zero second", "{}", now+10); err != nil {
		t.Fatalf("FoldUserTextRows: %v", err)
	}
	if items, err := s.ListItems("t-fold-claims"); err != nil || len(items) != 2 {
		t.Fatalf("items = %+v (%v), want both rows kept", items, err)
	}
	released, err := s.ReleaseUnsettledDiffReviewClaims()
	if err != nil || released != 1 {
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want only the other message's claim", released, err)
	}
	if got, err := s.GetDiffReviewComment(owner, "merged"); err != nil || got.Status != "sent" {
		t.Fatalf("merged = %+v (%v), want it sent", got, err)
	}
	if got, err := s.GetDiffReviewComment(owner, "other"); err != nil || got.Status != "draft" {
		t.Fatalf("other = %+v (%v), want it a draft", got, err)
	}
}
