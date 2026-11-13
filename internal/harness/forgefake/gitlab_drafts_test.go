package forgefake

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const draftFixture = `{
  "viewer": "octo",
  "repos": [{"forge": "gitlab", "project": "grp/drafts", "pulls": [
    {"number": 4, "title": "Drafts", "headSha": "4444444444444444444444444444444444444444",
     "draftNotes": [{"id": 77, "author": "octo", "note": "LGTM"}, {"id": 78, "author": "someone", "note": "theirs"}]}
  ]}]
}`

func draftIDs(t *testing.T, e *Engine) []int64 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []int64
	for _, draft := range e.repoOn("gitlab", "gitlab.com", "grp/drafts").pull(4).DraftNotes {
		ids = append(ids, draft.ID)
	}
	return ids
}

// A created draft answers its id and is pending until published or
// deleted by that id; a delete names exactly one draft, and publish makes
// the viewer's drafts a thread and a comment while another author's stay.
func TestGitLabDraftNotesCreateDeleteAndPublish(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, draftFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	const mr = "projects/grp%2Fdrafts/merge_requests/4"

	create := func(body string) int64 {
		t.Helper()
		got := glabSend(t, srv, "POST", mr+"/draft_notes", body)
		var draft struct {
			ID int64 `json:"id"`
		}
		if got.status != http.StatusCreated || json.Unmarshal([]byte(got.body), &draft) != nil || draft.ID == 0 {
			t.Fatalf("create %s = %+v", body, got)
		}
		return draft.ID
	}
	summary := create(`{"note":"LGTM"}`)
	line := create(`{"note":"nit","position":{"base_sha":"b","start_sha":"s","head_sha":"4444444444444444444444444444444444444444","old_path":"a.go","new_path":"a.go","position_type":"text","new_line":4}}`)
	if got := draftIDs(t, e); !slices.Equal(got, []int64{77, 78, summary, line}) {
		t.Fatalf("pending = %v", got)
	}

	if got := glabSend(t, srv, "DELETE", mr+"/draft_notes/"+strconv.FormatInt(summary, 10), ""); got.status != http.StatusNoContent {
		t.Fatalf("delete = %+v", got)
	}
	if got := glabSend(t, srv, "DELETE", mr+"/draft_notes/"+strconv.FormatInt(summary, 10), ""); got.status != http.StatusNotFound {
		t.Fatalf("second delete = %+v, want 404", got)
	}
	if got := glabSend(t, srv, "DELETE", mr+"/draft_notes/78", ""); got.status != http.StatusNotFound {
		t.Fatalf("delete of another author's draft = %+v, want 404", got)
	}
	if got := draftIDs(t, e); !slices.Equal(got, []int64{77, 78, line}) {
		t.Fatalf("pending after delete = %v, want the same-text draft 77 kept", got)
	}

	listed := func() []int64 {
		t.Helper()
		got := glabGET(t, srv, "gitlab.com", mr+"/draft_notes?per_page=100")
		var drafts []struct {
			ID int64 `json:"id"`
		}
		if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &drafts) != nil {
			t.Fatalf("list = %+v", got)
		}
		var ids []int64
		for _, draft := range drafts {
			ids = append(ids, draft.ID)
		}
		return ids
	}
	if got := listed(); !slices.Equal(got, []int64{77, line}) {
		t.Fatalf("listed = %v, want the viewer's pending drafts", got)
	}

	if got := glabSend(t, srv, "POST", mr+"/draft_notes/bulk_publish", ""); got.status != http.StatusNoContent {
		t.Fatalf("publish = %+v", got)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("listed after publish = %v, want none", got)
	}
	if got := draftIDs(t, e); !slices.Equal(got, []int64{78}) {
		t.Fatalf("pending after publish = %v, want only another author's", got)
	}
	discussions := glabGET(t, srv, "gitlab.com", mr+"/discussions?per_page=100")
	if !strings.Contains(discussions.body, `"new_path":"a.go"`) || !strings.Contains(discussions.body, `"body":"nit"`) || !strings.Contains(discussions.body, `"body":"LGTM"`) {
		t.Fatalf("discussions after publish = %s", discussions.body)
	}

	if got := glabSend(t, srv, "POST", mr+"/draft_notes", `{"note":"x","commit_id":"abc"}`); got.status != http.StatusNotFound || !lastInvocation(t, e).Unhandled {
		t.Fatalf("create with an unimplemented parameter = %+v, want unhandled", got)
	}
	if got := glabSend(t, srv, "POST", mr+"/draft_notes", `{"note":" "}`); got.status != http.StatusBadRequest {
		t.Fatalf("create with no note = %+v, want 400", got)
	}
}
