package jiraprovider

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

func TestCreate(t *testing.T) {
	f, p := createSite(t, false)
	f.On("POST", createPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"project":{"id":"10000"},"issuetype":{"id":"10002"},"summary":"Fix the login page",
			"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Steps"}]}]},
			"priority":{"id":"1"},"labels":["ui","web"],"customfield_10050":{"id":"22"}}}`)
		jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`)(w, r, b)
	})
	res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "Fix the login page", Fields: []tracker.FieldEdit{
		{Field: "description", Value: tracker.MarkdownEdit("Steps", "")},
		{Field: "priority", Value: tracker.TypedValues("high")},
		{Field: "labels", Value: tracker.PickValues("ui", "web")},
		{Field: "customfield_10050", Value: tracker.PickValues("22")},
	}}))
	if id, ok := res.Created(); !ok || id != "10001" || states(res) != "create:applied" {
		t.Fatalf("got %s", states(res))
	}
	if !slices.Equal(res.Parts[0].Changed, []tracker.Resource{tracker.CardOf("10001"), tracker.DetailOf("10001")}) {
		t.Errorf("changed %+v", res.Parts[0].Changed)
	}
}

// TestCreateWithImage proves the parts of a create whose Markdown embeds
// an image: the create names the issue whatever became of the upload
// and the edit that writes the image in after it.
func TestCreateWithImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		upload int
		finish int
		want   string
	}{
		{"all applied", 200, 204, "create:applied, upload:applied, edit:applied"},
		{"upload refused", 413, 204, "create:applied, upload:refused, edit:not sent"},
		{"upload unanswered", 502, 204, "create:applied, upload:unknown, edit:not sent"},
		{"finish refused", 200, 400, "create:applied, upload:applied, edit:refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := createSite(t, false)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.png"), pngBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			f.On("POST", createPath, jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`))
			f.On("POST", uploadPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if tc.upload != 200 {
					jiratest.Reply(tc.upload, `{"errorMessages":["too large"]}`)(w, r, b)
					return
				}
				jiratest.Reply(200, `[{"id":"501","filename":"a.png","mimeType":"image/png","size":30}]`)(w, r, b)
			})
			f.On("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Location", "https://api.media.example.com/file/0f1e2d3c-0000-4000-8000-000000000501/binary?token=signed")
				w.WriteHeader(http.StatusSeeOther)
			})
			f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if tc.finish != 204 {
					jiratest.Reply(tc.finish, `{"errorMessages":["no"]}`)(w, r, b)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "Shot", ImageDir: dir,
				Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit("![a](a.png)", "")}}}))
			if got := states(res); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if id, ok := res.Created(); !ok || id != "10001" || res.Parts[1].Issue != "10001" {
				t.Errorf("created %q %v", id, ok)
			}
			if n := f.Count("POST", createPath); n != 1 {
				t.Errorf("%d creates", n)
			}
		})
	}
}

// TestCreateLostAnswer proves a create whose answer was lost or did not
// read is unknown and names no issue, and is sent once.
func TestCreateLostAnswer(t *testing.T) {
	for _, answer := range []struct {
		status int
		body   string
	}{{500, `{}`}, {201, `<html>stored</html>`}, {201, `{"key":"WEB-1"}`}} {
		f, p := createSite(t, false)
		f.On("POST", createPath, jiratest.Reply(answer.status, answer.body))
		res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "Lost"}))
		if states(res) != "create:unknown" || res.Parts[0].Issue != "" || !res.CreateUnknown() {
			t.Errorf("%d %s: %s", answer.status, answer.body, states(res))
		}
		if n := f.Count("POST", createPath); n != 1 {
			t.Errorf("%d %s: %d creates", answer.status, answer.body, n)
		}
	}
}

// TestCreateRefusals proves each refusal reaches its own reason, and that
// nothing is sent for a refusal made before sending.
func TestCreateRefusals(t *testing.T) {
	in := func(edits ...tracker.FieldEdit) tracker.CreateInput {
		return tracker.CreateInput{Project: "10000", Type: "10002", Title: "New", Fields: edits}
	}
	for _, tc := range []struct {
		name     string
		in       tracker.CreateInput
		required bool
		state    tracker.State
		reason   tracker.Reason
		sent     int
	}{
		{"no match", in(tracker.FieldEdit{Field: "priority", Value: tracker.TypedValues("Urgent")}), false, tracker.StateRefused, tracker.ReasonNoMatch, 0},
		{"ambiguous", in(tracker.FieldEdit{Field: "customfield_10050", Value: tracker.TypedValues("minor")}), false, tracker.StateRefused, tracker.ReasonAmbiguous, 0},
		{"missing a required field", in(), true, tracker.StateRefused, tracker.ReasonMissing, 0},
		{"a read-only field", in(tracker.FieldEdit{Field: "customfield_10070", Value: tracker.PickValues("30")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"the sprint field", in(tracker.FieldEdit{Field: "customfield_10020", Value: tracker.PickValues("7")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"Markdown for an option", in(tracker.FieldEdit{Field: "priority", Value: tracker.MarkdownEdit("High", "")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"typed rich text", in(tracker.FieldEdit{Field: "description", Value: tracker.TypedValues("text")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"two values for one", in(tracker.FieldEdit{Field: "priority", Value: tracker.PickValues("1", "3")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"no such field", in(tracker.FieldEdit{Field: "customfield_99999", Value: tracker.PickValues("1")}), false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"a project that is not an ID", tracker.CreateInput{Project: "WEB", Type: "10002", Title: "New"}, false, tracker.StateRefused, tracker.ReasonInvalid, 0},
		{"refused by Jira", in(), false, tracker.StateRefused, tracker.ReasonTracker, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := createSite(t, tc.required)
			f.On("POST", createPath, jiratest.Reply(400, `{"errorMessages":[],"errors":{"summary":"Summary is too long"}}`))
			res := checked(t, p.Create(t.Context(), tc.in))
			if len(res.Parts) != 1 || res.Parts[0].Outcome.State() != tc.state || reason(res, 0) != tc.reason {
				t.Fatalf("got %s (reason %d): %v", states(res), reason(res, 0), res.Parts[0].Outcome.Err())
			}
			if n := f.Count("POST", createPath); n != tc.sent {
				t.Errorf("%d creates sent", n)
			}
			r, _ := res.Parts[0].Outcome.Refusal()
			switch tc.reason {
			case tracker.ReasonTracker:
				if !slices.Equal(r.Fields, []tracker.FieldMessage{{Field: tracker.FieldTitle, Message: "Summary is too long"}}) {
					t.Errorf("fields %+v", r.Fields)
				}
			case tracker.ReasonNoMatch:
				if len(r.Candidates) != 3 || r.Candidates[0] != (tracker.Choice{ID: "1", Name: "High"}) {
					t.Errorf("candidates %+v", r.Candidates)
				}
			case tracker.ReasonAmbiguous:
				if len(r.Candidates) != 2 {
					t.Errorf("candidates %+v", r.Candidates)
				}
			case tracker.ReasonMissing:
				if len(r.Fields) != 1 || r.Fields[0].Field != "customfield_10050" {
					t.Errorf("fields %+v", r.Fields)
				}
			case tracker.ReasonInvalid, tracker.ReasonUnsupported, tracker.ReasonStale, tracker.ReasonLoss:
				// The reason is the whole check.
			}
		})
	}
}

// TestCreateNotSent proves a create whose screen read failed, or whose
// call ended, is not sent rather than refused or unknown.
func TestCreateNotSent(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", createMetaPath, jiratest.Reply(500, `{}`))
	res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "New"}))
	if states(res) != "create:not sent" {
		t.Errorf("a failed screen read: %s", states(res))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res = checked(t, p.Create(ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "New"}))
	if states(res) != "create:not sent" {
		t.Errorf("a cancelled create: %s", states(res))
	}
	if n := f.Count("POST", createPath); n != 0 {
		t.Errorf("%d creates", n)
	}
}
