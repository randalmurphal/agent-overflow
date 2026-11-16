package git

import (
	"errors"
	"net/http"
	"slices"
	"testing"
)

// A GitLab Request changes publishes its comments as drafts and posts its
// summary as a thread to resolve, last, never as a draft. From 19.3 the
// publish carries reviewer_state requested_changes, and runs for the state
// alone when there is no comment; before 19.3 it carries no body, and a
// review with no comment sends no publish.
func TestGitLabRequestChangesPostsAThreadAndSetsTheState(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	line := 3
	comment := ReviewLineComment{Path: "a.go", Body: "nit", Line: &line, Side: "right"}
	for _, tc := range []struct {
		name, version string
		comments      []ReviewLineComment
		want          []string
		publishBody   string
	}{
		{"19.6 with a comment", "19.6.0-ee", []ReviewLineComment{comment},
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100", "GET " + mr + "/diffs?per_page=100", "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes/bulk_publish", "POST " + mr + "/discussions"},
			`{"reviewer_state":"requested_changes"}`},
		{"19.6 alone", "19.6.0-ee", nil,
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100", "POST " + mr + "/draft_notes/bulk_publish", "POST " + mr + "/discussions"},
			`{"reviewer_state":"requested_changes"}`},
		{"18.5 with a comment", "18.5.2", []ReviewLineComment{comment},
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100", "GET " + mr + "/diffs?per_page=100", "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes/bulk_publish", "POST " + mr + "/discussions"},
			``},
		{"18.5 alone", "18.5.2", nil,
			[]string{"GET metadata", "GET " + mr, "POST " + mr + "/discussions"},
			``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forge := &gitlabDraftForge{next: 100, pending: map[int64]string{}}
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "" && call.Path == "metadata":
					return gitlabMetadata(tc.version)
				case call.Method == http.MethodPost && call.Path == mr+"/discussions":
					return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":"abc","individual_note":false,"notes":[{"id":5,"type":"DiscussionNote","resolvable":true}]}`}
				}
				return forge.answer(t, mr)(call)
			})
			result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, HeadSHA: gitlabTestHead, Body: " Rename it. ", Comments: tc.comments})
			if err != nil || !result.PostedReview || result.PostedFileComments != 0 {
				t.Fatalf("submit = %+v, %v", result, err)
			}
			if got := gitlabPaths(calls); !slices.Equal(got, tc.want) {
				t.Fatalf("requests = %q\nwant %q", got, tc.want)
			}
			for _, call := range calls.all() {
				switch call.Path {
				case mr + "/draft_notes/bulk_publish":
					if string(call.Body) != tc.publishBody {
						t.Fatalf("publish body = %s, want %s", call.Body, tc.publishBody)
					}
				case mr + "/discussions":
					if string(call.Body) != `{"body":"Changes requested:\n\nRename it."}` {
						t.Fatalf("thread body = %s", call.Body)
					}
				case mr + "/draft_notes":
					if call.Method == http.MethodPost && string(call.Body) == `{"note":"Changes requested:\n\n Rename it. "}` {
						t.Fatal("the summary went out as a draft note")
					}
				}
			}
		})
	}
}

// A Request changes with no comment publishes the reviewer state from
// 19.3, and that publish would post the viewer's pending draft notes too,
// so one this submit did not create refuses it before any write. Before
// 19.3 the same review sends no publish and is not refused.
func TestGitLabRequestChangesAloneRefusesPendingDraftNotesWhereItPublishes(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	review := SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, HeadSHA: gitlabTestHead, Body: "Rename it."}
	for _, tc := range []struct {
		name, version string
		refused       bool
		want          []string
	}{
		{"19.6", "19.6.0-ee", true,
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100"}},
		{"18.5", "18.5.2", false,
			[]string{"GET metadata", "GET " + mr, "POST " + mr + "/discussions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forge := &gitlabDraftForge{next: 100, pending: map[int64]string{41: `{"note":"started on GitLab"}`}}
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "" && call.Path == "metadata":
					return gitlabMetadata(tc.version)
				case call.Method == http.MethodPost && call.Path == mr+"/discussions":
					return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":"abc","individual_note":false,"notes":[{"id":5,"type":"DiscussionNote","resolvable":true}]}`}
				}
				return forge.answer(t, mr)(call)
			})
			result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
			refused, isRefused := errors.AsType[*PendingDraftNotesError](err)
			switch {
			case tc.refused && (!isRefused || refused.Count != 1 || result.PostedReview):
				t.Fatalf("submit = %+v, %v; want a PendingDraftNotesError for 1", result, err)
			case !tc.refused && (err != nil || !result.PostedReview):
				t.Fatalf("submit = %+v, %v; want the thread posted", result, err)
			}
			if got := gitlabPaths(calls); !slices.Equal(got, tc.want) {
				t.Fatalf("requests = %q\nwant %q", got, tc.want)
			}
			if forge.publishes != 0 {
				t.Fatal("the submit published the viewer's pending draft")
			}
			if got := forge.pendingIDs(); !slices.Equal(got, []int64{41}) {
				t.Fatalf("pending drafts = %v, want the viewer's own untouched", got)
			}
		})
	}
}

// A publish of the reviewer state alone whose answer is lost leaves no
// draft to tell by: the submit is unconfirmed and posts no thread.
func TestGitLabRequestChangesLostStatePublishIsUnconfirmed(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	forge := &gitlabDraftForge{next: 100, pending: map[int64]string{}, publish500: "applied"}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "" && call.Path == "metadata" {
			return gitlabMetadata("19.6.0-ee")
		}
		return forge.answer(t, mr)(call)
	})
	_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, HeadSHA: gitlabTestHead, Body: "Rename it."})
	if _, ok := errors.AsType[*UnconfirmedSubmitError](err); !ok {
		t.Fatalf("submit = %v, want unconfirmed", err)
	}
	if n := countPrefix(gitlabPaths(calls), "POST "+mr+"/discussions"); n != 0 {
		t.Fatal("an unconfirmed publish went on to post the thread")
	}
}

// A thread whose answer is lost after the publish posted is a partial
// submit the forge may have applied, naming the thread; one GitLab refused
// is a partial submit it did not. A thread that is the submit's only
// write (18.5, no comment) fails as that write.
func TestGitLabRequestChangesThreadFailureSaysWhatApplied(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	for _, tc := range []struct {
		name, version string
		status        int
		partial       bool
		unconfirmed   bool
	}{
		{"lost after the state", "19.6.0-ee", http.StatusInternalServerError, true, true},
		{"refused after the state", "19.6.0-ee", http.StatusUnprocessableEntity, true, false},
		{"refused alone", "18.5.2", http.StatusUnprocessableEntity, false, false},
		{"lost alone", "18.5.2", http.StatusInternalServerError, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forge := &gitlabDraftForge{next: 100, pending: map[int64]string{}}
			core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "" && call.Path == "metadata":
					return gitlabMetadata(tc.version)
				case call.Method == http.MethodPost && call.Path == mr+"/discussions":
					return forgeAPIAnswer{Status: tc.status, Body: `{"message":"no"}`}
				}
				return forge.answer(t, mr)(call)
			})
			result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, HeadSHA: gitlabTestHead, Body: "Rename it."})
			partial, isPartial := errors.AsType[*PartialSubmitError](err)
			_, unconfirmed := errors.AsType[*UnconfirmedSubmitError](err)
			switch {
			case tc.partial && (!isPartial || !partial.PostedReview || partial.Verdict != SubmitStepChangesThread || partial.Unconfirmed != tc.unconfirmed || !result.PostedReview):
				t.Fatalf("submit = %+v, %v; want a partial naming the thread", result, err)
			case !tc.partial && (isPartial || err == nil || unconfirmed != tc.unconfirmed):
				t.Fatalf("submit = %+v, %v; want the thread's own failure", result, err)
			}
		})
	}
}
