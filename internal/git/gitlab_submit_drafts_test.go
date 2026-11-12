package git

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// gitlabDraftForge is a GitLab MR whose draft notes are state: POST creates
// one and answers its id, DELETE removes one by id, GET lists them. failAt
// refuses the n-th create with a 422 (1-based, 0 never), or answers it 500
// after creating it with failAtUnconfirmed; publishFails refuses
// bulk_publish with a 422; publish500 answers it 500, after publishing
// ("applied") or without ("ignored"); deleteFails answers every delete 500.
type gitlabDraftForge struct {
	mu                sync.Mutex
	next              int64
	pending           map[int64]string
	creates           int
	failAt            int
	failAtUnconfirmed bool
	publishFails      bool
	publish500        string
	deleteFails       bool
	publishes         int
}

const gitlabTest422 = `{"message":"422 Unprocessable Entity"}`
const gitlabTest500 = `{"message":"500 Internal Server Error"}`

func (f *gitlabDraftForge) answer(t *testing.T, mr string) func(call forgeAPICall) forgeAPIAnswer {
	return func(call forgeAPICall) forgeAPIAnswer {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := call.Method + " " + call.Path
		switch {
		case key == "GET "+mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case key == "POST "+mr+"/draft_notes":
			f.creates++
			if f.creates == f.failAt && !f.failAtUnconfirmed {
				return forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: gitlabTest422}
			}
			f.next++
			f.pending[f.next] = string(call.Body)
			if f.creates == f.failAt {
				return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: gitlabTest500}
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: fmt.Sprintf(`{"id":%d}`, f.next)}
		case key == "POST "+mr+"/draft_notes/bulk_publish":
			f.publishes++
			if f.publishFails {
				return forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: gitlabTest422}
			}
			if f.publish500 != "ignored" {
				clear(f.pending)
			}
			if f.publish500 != "" {
				return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: gitlabTest500}
			}
			return forgeAPIAnswer{Status: http.StatusNoContent}
		case call.Method == http.MethodGet && strings.HasPrefix(call.Path, mr+"/draft_notes?"):
			notes := make([]string, 0, len(f.pending))
			for id := range f.pending {
				notes = append(notes, fmt.Sprintf(`{"id":%d}`, id))
			}
			return forgeAPIAnswer{Body: "[" + strings.Join(notes, ",") + "]"}
		case call.Method == http.MethodDelete && strings.HasPrefix(call.Path, mr+"/draft_notes/"):
			if f.deleteFails {
				return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: gitlabTest500}
			}
			id, err := strconv.ParseInt(strings.TrimPrefix(call.Path, mr+"/draft_notes/"), 10, 64)
			if _, ok := f.pending[id]; err != nil || !ok {
				return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Not found"}`}
			}
			delete(f.pending, id)
			return forgeAPIAnswer{Status: http.StatusNoContent}
		}
		return forgeUnexpected(t, call)
	}
}

func (f *gitlabDraftForge) pendingIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.pending))
	for id := range f.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// A submit whose k-th of n requests fails removes exactly the drafts it
// created, so nothing it made is left pending and a pending draft the user
// already had, whatever its text, is untouched.
func TestGitLabSubmitReviewFailureRemovesTheDraftsItCreated(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	line, other := 4, 9
	// The user's own pending reply reads exactly like this submit's summary.
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "LGTM", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "nit", Line: &line, Side: "right"},
		{Path: "a.go", Body: "nit", Line: &other, Side: "right"},
	}}
	for name, forge := range map[string]*gitlabDraftForge{
		"second of three creates fails": {failAt: 2},
		"third of three creates fails":  {failAt: 3},
		"publish fails":                 {publishFails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forge.next = 100
			forge.pending = map[int64]string{100: `{"note":"LGTM"}`}
			core, _ := newForgeAPICore(t, forge.answer(t, mr))
			_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
			if err == nil {
				t.Fatal("SubmitReview succeeded, want the failure")
			}
			if _, left := errors.AsType[*DraftsLeftError](err); left {
				t.Fatalf("SubmitReview = %v, want no drafts left", err)
			}
			if got := forge.pendingIDs(); !slices.Equal(got, []int64{100}) {
				t.Fatalf("pending drafts = %v, want only the user's own 100", got)
			}
		})
	}
}

// A submit that fails and then cannot remove its drafts says so: the error
// carries how many may remain, and its explanation tells the user.
func TestGitLabSubmitReviewReportsDraftsItCouldNotRemove(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	line := 4
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "Summary", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "nit", Line: &line, Side: "right"},
	}}
	forge := &gitlabDraftForge{pending: map[int64]string{}, publishFails: true, deleteFails: true}
	core, _ := newForgeAPICore(t, forge.answer(t, mr))
	_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
	left, ok := errors.AsType[*DraftsLeftError](err)
	if !ok || left.Count != 2 || left.Cleanup == nil {
		t.Fatalf("SubmitReview = %#v, want a DraftsLeftError for both drafts", err)
	}
	explained, ok := errors.AsType[*ForgeActionError](ExplainForgeError(gitlabTestRef(7), ActionReview, err))
	if !ok || !strings.Contains(explained.Message, "2 draft comments") || !strings.Contains(explained.Message, "may still be pending") {
		t.Fatalf("explained = %#v, want it to say the drafts may remain", explained)
	}
	if got := forge.pendingIDs(); len(got) != 2 {
		t.Fatalf("pending drafts = %v, want the two the failed delete left", got)
	}
}

// A create the forge answered 500 may have made its note, so the submit
// says one may remain pending; it is still a submit that posted nothing.
func TestGitLabSubmitReviewUnconfirmedCreateMayLeaveADraft(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	line := 4
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "Summary", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "nit", Line: &line, Side: "right"},
	}}
	forge := &gitlabDraftForge{pending: map[int64]string{}, failAt: 2, failAtUnconfirmed: true}
	core, _ := newForgeAPICore(t, forge.answer(t, mr))
	_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
	left, ok := errors.AsType[*DraftsLeftError](err)
	if !ok || left.Count != 1 {
		t.Fatalf("SubmitReview = %#v, want a DraftsLeftError for the unconfirmed create", err)
	}
	if _, unconfirmed := errors.AsType[*UnconfirmedSubmitError](err); unconfirmed {
		t.Fatalf("SubmitReview = %v; a draft note is not a posted review", err)
	}
}

// A publish answered 500 is settled by the pending draft notes: gone means
// it published; still pending means it did not, and they are removed.
func TestGitLabSubmitReviewPublish500IsSettledByTheDraftNotes(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "Summary"}
	t.Run("applied", func(t *testing.T) {
		t.Parallel()
		forge := &gitlabDraftForge{pending: map[int64]string{}, publish500: "applied"}
		core, _ := newForgeAPICore(t, forge.answer(t, mr))
		out, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
		if err != nil || !out.PostedReview || forge.publishes != 1 {
			t.Fatalf("SubmitReview = %+v, %v after %d publishes; want posted once", out, err, forge.publishes)
		}
	})
	t.Run("ignored", func(t *testing.T) {
		t.Parallel()
		forge := &gitlabDraftForge{pending: map[int64]string{}, publish500: "ignored"}
		core, _ := newForgeAPICore(t, forge.answer(t, mr))
		_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
		if err == nil {
			t.Fatal("SubmitReview succeeded, want not posted")
		}
		if _, unconfirmed := errors.AsType[*UnconfirmedSubmitError](err); unconfirmed {
			t.Fatalf("SubmitReview = %v, want not posted", err)
		}
		if got := forge.pendingIDs(); len(got) != 0 {
			t.Fatalf("pending drafts = %v, want them removed", got)
		}
	})
}
