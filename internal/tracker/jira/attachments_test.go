package jira

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestAttachUploadsWithTheAntiXSRFHeader(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
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

func TestAttachmentsListAndDelete(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "attachment")
		reply(200, `{"id":"10001","key":"WEB-1","fields":{"attachment":[{"id":"501","filename":"a.png","size":3}]}}`)(w, r, b)
	})
	f.on("DELETE", "/rest/api/3/attachment/501", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	got, err := c.Attachments(t.Context(), "WEB-1")
	if err != nil || len(got) != 1 || got[0].Filename != "a.png" {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := c.DeleteAttachment(t.Context(), "501"); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadAttachmentIsBounded(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
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
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Location", "https://api.media.example.com/file/0f1e2d3c-media/binary?token=signed&client=x")
		w.WriteHeader(http.StatusSeeOther)
	})
	id, err := c.AttachmentMediaID(t.Context(), "501")
	if err != nil || id != "0f1e2d3c-media" {
		t.Fatalf("got %q %v", id, err)
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
