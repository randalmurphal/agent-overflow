package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestApplyCreateWithImage also proves every spelling of one file is one
// image, uploaded once.
func TestApplyCreateWithImage(t *testing.T) {
	f, c := createSite(t)
	dir := imageDir(t, map[string][]byte{"shots/a.png": pngBytes})
	plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "1", ImageDir: dir, Fields: []FieldInput{
		{Field: "Summary", Value: Text("With a picture")},
		{Field: "Description", Value: Text("See:\n\n![shot](/shots/a.png)\n\nand ![shot](shots/a.png) and ![shot](shots/../shots/a.png)\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pv := plan.Preview()
	if len(pv.Images) != 1 || pv.Images[0].Size != int64(len(pngBytes)) || pv.Images[0].Name != "a.png" {
		t.Fatalf("images %+v", pv.Images)
	}
	// The issue type is named by its ID, so only the create screen is read.
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("planning sent %d requests, want only the create screen read", n)
	}
	f.On("POST", "/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `[image: a.png]`) || strings.Contains(string(b), pendingPrefix) {
			t.Errorf("the create must carry the stand-in, not a placeholder: %s", b)
		}
		jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`)(w, r, b)
	})
	var uploads atomic.Int32
	imageRoutes(t, f, "10001", &uploads)
	f.On("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var body struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		_ = json.Unmarshal(b, &body)
		if len(body.Fields) != 1 || strings.Count(string(body.Fields["description"]), `"id":"0f1e2d3c-0000-4000-8000-000000000501"`) != 3 {
			t.Errorf("finish %s", b)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	res, err := c.ApplyCreate(t.Context(), plan)
	if err != nil || res.Key != "WEB-1" || len(res.Uploaded) != 1 || uploads.Load() != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if strings.Contains(string(plan.values[1].Fields), "0f1e2d3c-0000-4000-8000-000000000501") {
		t.Error("applying changed the plan")
	}
}

func TestApplyCreateImageFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failAt  string
		status  int
		step    Step
		unknown bool
		created bool
	}{
		{"image gone before the create", "", 0, StepCheck, false, false},
		{"upload refused after the create", "POST /rest/api/3/issue/10001/attachments", 413, StepUpload, false, true},
		{"finish with unknown outcome", "PUT /rest/api/3/issue/10001", 500, StepFinish, true, true},
		{"finish answered 200, which it does not document", "PUT /rest/api/3/issue/10001", 200, StepFinish, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := createSite(t)
			dir := imageDir(t, map[string][]byte{"a.png": pngBytes})
			plan, err := c.PlanCreate(t.Context(), CreateInput{Project: "WEB", IssueType: "Task", ImageDir: dir,
				Fields: []FieldInput{{Field: "Summary", Value: Text("x")}, {Field: "Description", Value: Text("![a](a.png)")}}})
			if err != nil {
				t.Fatal(err)
			}
			f.On("POST", "/rest/api/3/issue", jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`))
			var uploads atomic.Int32
			imageRoutes(t, f, "10001", &uploads)
			f.On("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
			if tc.failAt == "" {
				if err := os.Remove(filepath.Join(dir, "a.png")); err != nil {
					t.Fatal(err)
				}
			} else {
				method, path, _ := strings.Cut(tc.failAt, " ")
				f.On(method, path, jiratest.Reply(tc.status, `{"errorMessages":["no"]}`))
			}
			res, err := c.ApplyCreate(t.Context(), plan)
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != tc.step || se.OutcomeUnknown() != tc.unknown {
				t.Fatalf("got %v", err)
			}
			if (res.Key == "WEB-1") != tc.created || (f.Count("POST", "/rest/api/3/issue") == 1) != tc.created {
				t.Errorf("created %v, want %v", res.Key, tc.created)
			}
		})
	}
}

func TestApplyEditImageFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upload    int    // status of the second upload
		answer    string // the second upload's answer when it is a 200; empty names one attachment
		media     int    // status of the second media ID read; zero redirects
		put       int
		step      Step // empty when the edit succeeds
		uploaded  int
		unknown   bool
		wantPuts  int
		changeImg bool
	}{
		{name: "second upload fails before the write", upload: 500, put: 204, step: StepUpload, uploaded: 1, unknown: true},
		{name: "second media ID read fails", upload: 200, media: 500, put: 204, step: StepMediaID, uploaded: 2},
		{name: "second upload stores two attachments", upload: 200, answer: `[{"id":"502"},{"id":"503"}]`, put: 204, step: StepEdit, uploaded: 3},
		{name: "second upload names its attachment and does not decode", upload: 200, answer: `[{"id":"502","size":"big"}]`, put: 204, uploaded: 2, wantPuts: 1},
		{name: "write refused after the uploads", upload: 200, put: 400, step: StepEdit, uploaded: 2, wantPuts: 1},
		{name: "write outcome unknown", upload: 200, put: 502, step: StepEdit, uploaded: 2, unknown: true, wantPuts: 1},
		{name: "second image changed since the plan", changeImg: true, step: StepCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := storedDesc(t, adf.Doc(para("old")))
			f, c := editSite(t, desc)
			dir := imageDir(t, map[string][]byte{"a.png": pngBytes, "b.png": pngBytes})
			plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", ImageDir: dir,
				Fields: []FieldInput{{Field: "description", Value: Text("![a](a.png) ![b](b.png)")}}})
			if err != nil || len(plan.Preview().Images) != 2 {
				t.Fatalf("plan %v %v", plan, err)
			}
			if tc.changeImg {
				// The same size, so only the content tells it changed.
				changed := append([]byte(nil), pngBytes...)
				changed[len(changed)-1] ^= 0xff
				if err := os.WriteFile(filepath.Join(dir, "b.png"), changed, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var uploads atomic.Int32
			f.On("POST", "/rest/api/3/issue/10001/attachments", func(w http.ResponseWriter, r *http.Request, b []byte) {
				n := uploads.Add(1)
				switch {
				case n == 2 && tc.upload != 200:
					jiratest.Reply(tc.upload, `{}`)(w, r, b)
				case n == 2 && tc.answer != "":
					jiratest.Reply(200, tc.answer)(w, r, b)
				default:
					jiratest.Reply(200, `[{"id":"50`+string('0'+rune(n))+`","filename":"x.png"}]`)(w, r, b)
				}
			})
			mediaRoutes(f)
			if tc.media != 0 {
				f.On("GET", "/rest/api/3/attachment/content/502", jiratest.Reply(tc.media, `{}`))
			}
			f.On("PUT", "/rest/api/3/issue/10001", jiratest.Reply(tc.put, `{"errorMessages":["x"]}`))
			res, err := c.ApplyEdit(t.Context(), plan)
			if len(res.Uploaded) != tc.uploaded {
				t.Errorf("uploaded %+v, want %d", res.Uploaded, tc.uploaded)
			}
			if tc.step == "" {
				if err != nil {
					t.Fatalf("got %v", err)
				}
				return
			}
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != tc.step || se.OutcomeUnknown() != tc.unknown {
				t.Fatalf("got %+v, %v (unknown %v)", res, err, ok && se.OutcomeUnknown())
			}
			if tc.step == StepMediaID && (!trackerapi.WriteNotApplied(err) || errors.Is(err, trackerapi.ErrInvalidInput)) {
				t.Errorf("a failed media ID read must leave the edit not sent, not refused: %v", err)
			}
			if tc.changeImg {
				if _, stale := errors.AsType[*StaleError](err); !stale || uploads.Load() != 0 || !trackerapi.WriteNotApplied(err) {
					t.Errorf("a changed image: %v after %d uploads", err, uploads.Load())
				}
			}
			if n := f.Count("PUT", "/rest/api/3/issue/10001"); n != tc.wantPuts {
				t.Errorf("%d writes, want %d", n, tc.wantPuts)
			}
		})
	}
}

// TestImageFolderIsConfined proves the image folder is enforced by the
// package, whatever the names and links inside it say.
func TestImageFolderIsConfined(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	_, c := editSite(t, desc)
	outside := imageDir(t, map[string][]byte{"private.png": pngBytes, "dir/private.png": pngBytes})
	dir := imageDir(t, map[string][]byte{"in.png": pngBytes})
	for name, target := range map[string]string{
		"escape.png": filepath.Join(outside, "private.png"),
		"linked":     filepath.Join(outside, "dir"),
		"inside.png": "in.png",
		"absolute":   filepath.Join(dir, "in.png"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symbolic links: %v", err)
		}
	}
	plan := func(dir, ref string) error {
		_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", ImageDir: dir, Fields: []FieldInput{{Field: "description", Value: Text("![x](" + ref + ")")}}})
		return err
	}
	for name, ref := range map[string]string{
		"a link to a file outside":        "escape.png",
		"a linked directory outside":      "linked/private.png",
		"dot dot":                         "../" + filepath.Base(outside) + "/private.png",
		"an absolute path":                filepath.ToSlash(filepath.Join(outside, "private.png")),
		"a path through a name to escape": "in.png/../../" + filepath.Base(outside) + "/private.png",
		// os.Root refuses a link to an absolute path, even one inside.
		"an absolute link inside": "absolute",
	} {
		if err := plan(dir, ref); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s (%s): %v", name, ref, err)
		}
	}
	if err := plan(dir, "inside.png"); err != nil {
		t.Errorf("a link inside the folder: %v", err)
	}
	// A name that leaves the folder is refused by its spelling, before
	// any folder is opened, so even one with no folder says why.
	for _, ref := range []string{"../a.png", "in.png/../../a.png", "/", "."} {
		if err := plan("", ref); !strings.Contains(fmt.Sprint(err), "not a path inside the image folder") {
			t.Errorf("%s: %v", ref, err)
		}
	}
	// The relative folder names the real folder from the working
	// directory, so only its being relative refuses it.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]string{"no folder": "", "a relative folder": rel} {
		if err := plan(d, "in.png"); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPlanImageChecks(t *testing.T) {
	desc := storedDesc(t, adf.Doc(para("old")))
	_, c := editSite(t, desc)
	half := make([]byte, 3<<10)
	copy(half, pngBytes)
	dir := imageDir(t, map[string][]byte{
		"notes.txt": []byte("plain text"),
		"big.png":   append(append([]byte(nil), pngBytes...), make([]byte, 8<<10)...),
		"h1.png":    half, "h2.png": half,
	})
	c.maxImages = 4 << 10
	for name, md := range map[string]string{
		"not an image":                 "![n](notes.txt)",
		"missing":                      "![m](nowhere.png)",
		"over the limit":               "![b](big.png)",
		"together over the limit":      "![1](h1.png) ![2](h2.png)",
		"the folder itself":            "![d](/)",
		"blank":                        "<!-- only a comment -->",
		"an image outside the folder ": "![o](../a.png)",
	} {
		_, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", ImageDir: dir, Fields: []FieldInput{{Field: "description", Value: Text(md)}}})
		if !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", ImageDir: dir, Fields: []FieldInput{{Field: "description", Value: Text("![1](h1.png) ![again](/h1.png)")}}}); err != nil {
		t.Errorf("one image named twice counts once: %v", err)
	}
}
