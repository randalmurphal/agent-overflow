package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mintForge(t *testing.T, f *serverFixture, contentID string) string {
	t.Helper()
	relative, err := f.srv.MintForgeAttachmentTicket(contentID)
	if err != nil {
		t.Fatalf("mint forge ticket: %v", err)
	}
	return relative
}

// TestForgeAttachmentServesTheTicketedBytes is the happy path plus the
// URL shape: the content id is in the path, the ticket is in the query,
// and the whole thing lives under /attachments/ so every relay carries it.
func TestForgeAttachmentServesTheTicketedBytes(t *testing.T) {
	payload := []byte("MP4BYTES-and-more")
	stub := &stubTransfer{content: payload, mime: "video/mp4", forgeKind: "video"}
	f := attachmentFixture(t, stub)

	relative := mintForge(t, f, "Abc_123-xyz")
	if !strings.HasPrefix(relative, "/attachments/forge/Abc_123-xyz?ticket=") {
		t.Fatalf("minted URL %q does not name the content it admits", relative)
	}

	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != "" {
		t.Fatalf("Content-Disposition = %q; media renders inline", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload) {
		t.Fatalf("served %q, want %q", body, payload)
	}
}

// TestForgeAttachmentAnswersRange: a <video> seeks, so the route has to
// answer 206 — which it does because the seam hands over a seekable
// reader and http.ServeContent does the rest.
func TestForgeAttachmentAnswersRange(t *testing.T) {
	stub := &stubTransfer{content: []byte("0123456789"), mime: "video/mp4", forgeKind: "video"}
	f := attachmentFixture(t, stub)

	resp := get(t, f.srv.Addr(), mintForge(t, f, "vid"), map[string]string{"Range": "bytes=2-5"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q, want bytes 2-5/10", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "2345" {
		t.Fatalf("body = %q, want 2345", body)
	}
}

// TestForgeAttachmentFileKindIsNeverRenderable is the posture rule. The
// route answers at the SPA origin, so a payload nothing painted leaves
// as an opaque download whatever its own type claimed.
func TestForgeAttachmentFileKindIsNeverRenderable(t *testing.T) {
	stub := &stubTransfer{
		content:       []byte("<html><script>alert(1)</script>"),
		mime:          "text/html; charset=utf-8",
		forgeKind:     "file",
		forgeFilename: "Отчёт \"final\".html",
	}
	f := attachmentFixture(t, stub)

	resp := get(t, f.srv.Addr(), mintForge(t, f, "doc"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q; a file must never leave under its own type", got)
	}
	disposition := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want an attachment disposition", disposition)
	}
	// Both spellings: the quoted ASCII fallback every client reads, and
	// the extended form that can carry the real name.
	if !strings.Contains(disposition, "filename=") {
		t.Fatalf("Content-Disposition = %q, want an ASCII filename fallback", disposition)
	}
	if !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q, want an RFC 5987 filename*", disposition)
	}
	if strings.Contains(disposition, `"final"`) {
		t.Fatalf("Content-Disposition = %q leaves an unescaped quote in the parameter", disposition)
	}
}

func TestForgeAttachmentDispositionHeaders(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		want     string
	}{
		{name: "plain ascii needs no extended form", filename: "report.pdf", want: `attachment; filename=report.pdf`},
		{name: "empty name falls back", filename: "", want: `attachment; filename=download`},
		{name: "only the last path component survives", filename: "../../etc/passwd", want: `attachment; filename=passwd`},
		{name: "a space is quoted and needs no extended form", filename: "Screen Shot.png", want: `attachment; filename="Screen Shot.png"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentDisposition(tc.filename); got != tc.want {
				t.Fatalf("attachmentDisposition(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
	if got := attachmentDisposition("café.png"); got != `attachment; filename=caf_.png; filename*=UTF-8''caf%C3%A9.png` {
		t.Fatalf("non-ascii disposition = %q", got)
	}
}

// TestForgeAttachmentTicketIsSpentOnce and its siblings: every refusal is
// the same unfingerprintable 404, and none of them reach the seam.
func TestForgeAttachmentRefusals(t *testing.T) {
	t.Run("spent ticket", func(t *testing.T) {
		stub := &stubTransfer{content: []byte("x")}
		f := attachmentFixture(t, stub)
		relative := mintForge(t, f, "id-1")
		if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("first status = %d, want 200", resp.StatusCode)
		}
		if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("replayed status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("path disagrees with the subject", func(t *testing.T) {
		stub := &stubTransfer{content: []byte("x")}
		f := attachmentFixture(t, stub)
		relative := mintForge(t, f, "id-1")
		ticket := relative[strings.Index(relative, "?"):]
		resp := get(t, f.srv.Addr(), "/attachments/forge/id-2"+ticket, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if len(stub.opened) != 0 {
			t.Fatalf("the seam was reached for %v despite the mismatch", stub.opened)
		}
	})

	t.Run("no ticket at all", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: []byte("x")})
		if resp := get(t, f.srv.Addr(), "/attachments/forge/id-1", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("a download ticket is not a forge ticket", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: []byte("x")})
		relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		ticket := relative[strings.Index(relative, "?"):]
		if resp := get(t, f.srv.Addr(), "/attachments/forge/att-1"+ticket, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: the books must not be interchangeable", resp.StatusCode)
		}
	})

	t.Run("content the cache no longer holds", func(t *testing.T) {
		stub := &stubTransfer{openErr: errors.New("no longer cached")}
		f := attachmentFixture(t, stub)
		if resp := get(t, f.srv.Addr(), mintForge(t, f, "gone"), nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("no seam", func(t *testing.T) {
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AttachmentTransfer = nil })
		relative := mintForge(t, f, "id-1")
		if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	if _, err := (&Server{forgeAttachmentTickets: newTicketBook(4, attachmentTicketTTL)}).MintForgeAttachmentTicket(""); err == nil {
		t.Fatal("MintForgeAttachmentTicket accepted an empty content id")
	}
}

// TestForgeAttachmentPatternDoesNotShadowTheDownloadRoute: the two GET
// patterns overlap, and the mux has to prefer the literal `forge`
// segment while still routing a two-id path to the thread-attachment
// handler. A conflict here would panic at registration; a wrong
// preference would silently serve the wrong thing.
func TestForgeAttachmentPatternDoesNotShadowTheDownloadRoute(t *testing.T) {
	stub := &stubTransfer{content: []byte("bytes")}
	f := attachmentFixture(t, stub)

	if resp := get(t, f.srv.Addr(), mintForge(t, f, "content-1"), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("forge route status = %d, want 200", resp.StatusCode)
	}
	relative, err := f.srv.MintAttachmentDownloadTicket("3f1b9c2a-thread", "8d2e4f6a-att")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("thread download status = %d, want 200", resp.StatusCode)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	want := []string{"forge/content-1", "3f1b9c2a-thread/8d2e4f6a-att"}
	if fmt.Sprint(stub.opened) != fmt.Sprint(want) {
		t.Fatalf("the seam saw %v, want %v", stub.opened, want)
	}
}
