package jiraprovider

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// twoImagesMD embeds the two images twoImages writes.
const twoImagesMD = "![a](a.png) ![b](b.png)"

// What becomes of the second of two images' uploads.
const (
	secondStored  = "stored"
	secondRefused = "refused"
	secondMediaID = "media ID fails"
)

// twoImages writes a.png and b.png to a new directory, which it returns,
// and serves issue 10001's attach requests: the first stores attachment
// 501, the second stores 502, is refused, or stores 502 whose media ID
// read fails, as second says.
func twoImages(t *testing.T, f *jiratest.Server, second string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), pngBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var n atomic.Int32
	f.On("POST", uploadPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if n.Add(1) == 2 && second == secondRefused {
			jiratest.Reply(413, `{"errorMessages":["too large"]}`)(w, r, b)
			return
		}
		id := 500 + n.Load()
		jiratest.Reply(200, `[{"id":"`+decimal(int64(id))+`","filename":"x.png","mimeType":"image/png","size":30}]`)(w, r, b)
	})
	for _, id := range []string{"501", "502"} {
		f.On("GET", "/rest/api/3/attachment/content/"+id, func(w http.ResponseWriter, r *http.Request, b []byte) {
			if id == "502" && second == secondMediaID {
				jiratest.Reply(500, `{}`)(w, r, b)
				return
			}
			w.Header().Set("Location", "https://api.media.example.com/file/0f1e2d3c-0000-4000-8000-000000000"+id+"/binary?token=signed")
			w.WriteHeader(http.StatusSeeOther)
		})
	}
	return dir
}

// imageCases are what each write with two images reports, for its main
// step, when the second image is stored, refused or its media ID read
// fails: the stored image's upload applied, the refused attach its own
// upload part, and the write itself not sent after either failure.
func imageCases(step string) map[string]string {
	return map[string]string{
		secondStored:  "upload:applied, " + step + ":applied",
		secondRefused: "upload:applied, upload:refused, " + step + ":not sent",
		secondMediaID: "upload:applied, " + step + ":not sent",
	}
}

// TestImageUploadOutcomes proves that on every write that uploads the
// images its Markdown embeds, an image already stored is reported
// applied whatever became of a later one, a refused attach is its own
// refused upload part, and the write that would have followed is not
// sent, with the failure it was not sent for.
func TestImageUploadOutcomes(t *testing.T) {
	old := adf.ToMarkdown(adf.Doc(para("old")))
	writes := []struct {
		name, step string
		write      string // the write request
		run        func(t *testing.T, f *jiratest.Server, p *Provider, dir string) tracker.Result
	}{
		{"edit", "edit", "PUT " + issuePath, func(t *testing.T, f *jiratest.Server, p *Provider, dir string) tracker.Result {
			f.On("PUT", issuePath, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
			return p.Edit(t.Context(), tracker.EditInput{Issue: "10001", ImageDir: dir, Fields: []tracker.FieldEdit{
				{Field: "description", Value: tracker.MarkdownEdit(twoImagesMD, old)},
			}})
		}},
		{"move", "move", "POST /rest/api/3/issue/10001/transitions", func(t *testing.T, f *jiratest.Server, p *Provider, dir string) tracker.Result {
			f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
				{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
				 "description":`+metaFields["description"]+`}}]}`))
			f.On("POST", "/rest/api/3/issue/10001/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
			return p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", ImageDir: dir, Fields: []tracker.FieldEdit{
				{Field: "description", Value: tracker.MarkdownEdit(twoImagesMD, old)},
			}})
		}},
		{"comment", "comment", "POST " + commentsPath, func(t *testing.T, f *jiratest.Server, p *Provider, dir string) tracker.Result {
			f.On("GET", issuePath, jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","projectTypeKey":"software"}}}`))
			f.On("POST", commentsPath, storedComment(201, adf.Doc(para("x"))))
			return p.AddComment(t.Context(), tracker.CommentInput{Issue: "10001", Markdown: twoImagesMD, ImageDir: dir})
		}},
		{"comment edit", "comment", "PUT " + commentsPath + "/900", func(t *testing.T, f *jiratest.Server, p *Provider, dir string) tracker.Result {
			f.On("GET", commentsPath+"/900", storedComment(200, adf.Doc(para("old"))))
			f.On("PUT", commentsPath+"/900", storedComment(200, adf.Doc(para("x"))))
			return p.EditComment(t.Context(), tracker.CommentEdit{Issue: "10001", Comment: "900", Markdown: twoImagesMD, From: old, ImageDir: dir})
		}},
	}
	for _, w := range writes {
		for second, want := range imageCases(w.step) {
			t.Run(w.name+"/"+second, func(t *testing.T) {
				f, p := editSite(t, storedDesc(adf.Doc(para("old"))))
				dir := twoImages(t, f, second)
				res := checked(t, w.run(t, f, p, dir))
				if got := states(res); got != want {
					t.Fatalf("got %s, want %s: %v", got, want, res.Parts[len(res.Parts)-1].Outcome.Err())
				}
				method, path, _ := strings.Cut(w.write, " ")
				if n := f.Count(method, path); (n == 1) != (second == secondStored) {
					t.Errorf("%d writes", n)
				}
				if second != secondStored && res.Parts[len(res.Parts)-1].Outcome.Err() == nil {
					t.Error("the write not sent carries no failure")
				}
			})
		}
	}
}

// TestCreateImageUploadOutcomes proves the same of a create: the create
// is applied and names the issue, then the image parts and the edit that
// writes the images in.
func TestCreateImageUploadOutcomes(t *testing.T) {
	for second, want := range imageCases("edit") {
		t.Run(second, func(t *testing.T) {
			f, p := createSite(t, false)
			dir := twoImages(t, f, second)
			f.On("POST", createPath, jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`))
			f.On("PUT", issuePath, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
			res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "Shot", ImageDir: dir,
				Fields: []tracker.FieldEdit{{Field: "description", Value: tracker.MarkdownEdit(twoImagesMD, "")}}}))
			if got := states(res); got != "create:applied, "+want {
				t.Fatalf("got %s, want create:applied, %s", got, want)
			}
			if id, ok := res.Created(); !ok || id != "10001" {
				t.Errorf("created %q %v", id, ok)
			}
			if n := f.Count("PUT", issuePath); (n == 1) != (second == secondStored) {
				t.Errorf("%d edits", n)
			}
		})
	}
}
