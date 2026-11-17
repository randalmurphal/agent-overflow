package jira

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

func TestAttachUploadsWithTheAntiXSRFHeader(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/issue/WEB-1/attachments", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Header.Get("X-Atlassian-Token") != "no-check" {
			t.Error("no anti-XSRF header")
		}
		name, content := multipartFile(t, r, b)
		if name != "log.txt" || string(content) != "line one\n" {
			t.Errorf("uploaded %q %q", name, content)
		}
		reply(200, `[{"id":"501","filename":"log.txt","mimeType":"text/plain","size":9,"created":"2026-11-16T10:00:00.000+0000","author":`+userOneJSON+`}]`)(w, r, b)
	})
	got, err := c.Attach(t.Context(), "WEB-1", File{Name: "log.txt", Content: strings.NewReader("line one\n")})
	if err != nil || len(got) != 1 || got[0].ID != "501" || got[0].Author == nil || got[0].Created.IsZero() {
		t.Fatalf("got %+v %v", got, err)
	}
	c.maxUpload = 1 << 10
	_, err = c.Attach(t.Context(), "WEB-1", File{Name: "big.bin", Content: bytes.NewReader(make([]byte, 2<<10))})
	if !errors.Is(err, trackerapi.ErrUploadTooLarge) || !trackerapi.WriteNotApplied(err) {
		t.Errorf("an upload over the limit: %v", err)
	}
	for _, bad := range []File{{Name: "", Content: strings.NewReader("x")}, {Name: "a\nb", Content: strings.NewReader("x")}, {Name: "x"}} {
		if _, err := c.Attach(t.Context(), "WEB-1", bad); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if n := f.count("POST", "/rest/api/3/issue/WEB-1/attachments"); n != 1 {
		t.Errorf("%d uploads sent", n)
	}
}

// TestAttachUnreadableAnswerKeepsTheIDs proves an upload Jira stored
// whose answer does not decode still names what was stored, by ID.
func TestAttachUnreadableAnswerKeepsTheIDs(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/issue/WEB-1/attachments", reply(200, `[{"id":"501","size":"big"},{"id":502,"size":"big"}]`))
	got, err := c.Attach(t.Context(), "WEB-1", File{Name: "a.txt", Content: strings.NewReader("a")})
	se, ok := errors.AsType[*StepError](err)
	if !ok || !se.Applied() || len(got) != 2 || got[0].ID != "501" || got[1].ID != "502" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestAttachmentsListAndDelete(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "attachment")
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"attachment":[{"id":"501","filename":"a.png","size":3,"created":"2026-11-16T09:00:00.000+0000"},
			{"id":"502","filename":"b.png","size":4,"created":"yesterday"},{"id":"503","filename":"c.png","size":5}]}}`)(w, r, b)
	})
	f.on("DELETE", "/rest/api/3/attachment/501", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	got, err := c.Attachments(t.Context(), "WEB-1")
	if err != nil || len(got) != 3 || got[0].Filename != "a.png" || got[0].Created.IsZero() || len(got[0].Problems) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
	// A timestamp that does not parse is the record's problem; an absent
	// one is none.
	if got[1].Filename != "b.png" || problemIn(got[1].Problems, "created") == nil || len(got[2].Problems) != 0 {
		t.Errorf("got %+v", got[1:])
	}
	if err := c.DeleteAttachment(t.Context(), "501"); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadAttachmentIsBounded(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "redirect", "false")
		_, _ = w.Write([]byte("0123456789"))
	})
	var buf bytes.Buffer
	if err := c.DownloadAttachment(t.Context(), "501", &buf, 10); err != nil || buf.String() != "0123456789" {
		t.Fatalf("got %q %v", buf.String(), err)
	}
	buf.Reset()
	err := c.DownloadAttachment(t.Context(), "501", &buf, 4)
	if _, ok := errors.AsType[*trackerapi.BodyTooLargeError](err); !ok || buf.Len() > 4 {
		t.Errorf("over the limit: %v after %d bytes", err, buf.Len())
	}
	f.on("GET", "/rest/api/3/attachment/content/502", reply(404, `{}`))
	f.on("GET", myselfURL, reply(200, userOneJSON))
	if err := c.DownloadAttachment(t.Context(), "502", &buf, 10); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestAttachmentMediaID(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	const media = "0f1e2d3c-4b5a-4c6d-8e7f-a0b1c2d3e4f5"
	f.on("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Location", "https://api.media.example.com/file/"+media+"/binary?token=signed&client=x")
		w.WriteHeader(http.StatusSeeOther)
	})
	id, err := c.AttachmentMediaID(t.Context(), "501")
	if err != nil || id != media {
		t.Fatalf("got %q %v", id, err)
	}
	// Only a canonical UUID is a media ID, so a segment that carries the
	// credential, or any other text, is neither returned nor quoted.
	for _, seg := range []string{testToken, "0f1e2d3c-media", media + "x", "0f1e2d3c4b5a4c6d8e7fa0b1c2d3e4f5"} {
		f.on("GET", "/rest/api/3/attachment/content/504", func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Location", "https://api.media.example.com/file/"+seg+"/binary")
			w.WriteHeader(http.StatusSeeOther)
		})
		got, err := c.AttachmentMediaID(t.Context(), "504")
		if err == nil || got != "" || strings.Contains(err.Error(), seg) {
			t.Errorf("%s: got %q %v", seg, got, err)
		}
	}
	// A credential shaped like a media ID is scrubbed from the redirect
	// before it is read, so it fails the check and is never returned.
	uuidSecret, err := New(Options{
		BaseURL: f.srv.URL, Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte(media)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uuidSecret.Close)
	got, err := uuidSecret.AttachmentMediaID(t.Context(), "501")
	if err == nil || got != "" || strings.Contains(err.Error(), media) {
		t.Errorf("a media ID that is the credential: got %q %v", got, err)
	}
	f.on("GET", "/rest/api/3/attachment/content/502", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Location", "https://api.media.example.com/elsewhere")
		w.WriteHeader(http.StatusSeeOther)
	})
	if _, err := c.AttachmentMediaID(t.Context(), "502"); err == nil {
		t.Error("a redirect naming no media file")
	}
	f.on("GET", "/rest/api/3/attachment/content/503", reply(200, `{}`))
	if _, err := c.AttachmentMediaID(t.Context(), "503"); err == nil {
		t.Error("an answer that is not a redirect")
	}
}
