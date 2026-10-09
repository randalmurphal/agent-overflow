package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mintLocalImage(t *testing.T, f *serverFixture, contentID string) string {
	t.Helper()
	relative, err := f.srv.MintLocalImageTicket(contentID)
	if err != nil {
		t.Fatalf("mint local image ticket: %v", err)
	}
	return relative
}

func TestLocalImageServesTheTicketedBytes(t *testing.T) {
	payload := pngPayload()
	stub := &stubTransfer{content: payload, mime: "image/png"}
	f := attachmentFixture(t, stub)

	relative := mintLocalImage(t, f, "Abc_123-xyz")
	if !strings.HasPrefix(relative, "/attachments/image/Abc_123-xyz?ticket=") {
		t.Fatalf("minted URL %q does not name the content it admits", relative)
	}
	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":           "image/png",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "",
		"Last-Modified":          "Tue, 14 Nov 2023 22:13:20 GMT",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("no Content-Security-Policy on an attachment response")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload) {
		t.Fatalf("served %q, want %q", body, payload)
	}
}

func TestLocalImageAnswersRange(t *testing.T) {
	stub := &stubTransfer{content: []byte("0123456789"), mime: "image/png"}
	f := attachmentFixture(t, stub)

	resp := get(t, f.srv.Addr(), mintLocalImage(t, f, "img"), map[string]string{"Range": "bytes=2-5"})
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

// SVG is the one image type that is also a document. A fetch reads it
// either way; a navigation to the URL must download rather than render at
// the SPA origin. Both image routes answer it the same way.
func TestImageRoutesServeSVGAsADownload(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	t.Run("local image", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: svg, mime: "image/svg+xml"})
		resp := get(t, f.srv.Addr(), mintLocalImage(t, f, "svg"), nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("status %d, type %q, want 200 image/svg+xml", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if got := resp.Header.Get("Content-Disposition"); got != "attachment" {
			t.Fatalf("Content-Disposition = %q, want attachment", got)
		}
	})
	t.Run("forge attachment", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: svg, mime: "image/svg+xml", forgeKind: "image", forgeFilename: "diagram.svg"})
		resp := get(t, f.srv.Addr(), mintForge(t, f, "svg"), nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("status %d, type %q, want 200 image/svg+xml", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if got := resp.Header.Get("Content-Disposition"); got != "attachment; filename=diagram.svg" {
			t.Fatalf("Content-Disposition = %q, want an attachment naming the file", got)
		}
	})
	t.Run("a raster image still renders inline", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: pngPayload(), mime: "image/png", forgeKind: "image"})
		if got := get(t, f.srv.Addr(), mintForge(t, f, "png"), nil).Header.Get("Content-Disposition"); got != "" {
			t.Fatalf("forge png Content-Disposition = %q, want none", got)
		}
	})
}

// Every refusal is the same unfingerprintable 404, and the ones the ticket
// decides never reach the seam.
func TestLocalImageRefusals(t *testing.T) {
	t.Run("spent ticket", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{content: []byte("x")})
		relative := mintLocalImage(t, f, "id-1")
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
		relative := mintLocalImage(t, f, "id-1")
		ticket := relative[strings.Index(relative, "?"):]
		if resp := get(t, f.srv.Addr(), "/attachments/image/id-2"+ticket, nil); resp.StatusCode != http.StatusNotFound {
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
		if resp := get(t, f.srv.Addr(), "/attachments/image/id-1", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("the books are not interchangeable", func(t *testing.T) {
		stub := &stubTransfer{content: []byte("x")}
		f := attachmentFixture(t, stub)
		forge := mintForge(t, f, "shared-id")
		if resp := get(t, f.srv.Addr(), "/attachments/image/shared-id"+forge[strings.Index(forge, "?"):], nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("a forge ticket opened a local image: status %d", resp.StatusCode)
		}
		image := mintLocalImage(t, f, "shared-id")
		if resp := get(t, f.srv.Addr(), "/attachments/forge/shared-id"+image[strings.Index(image, "?"):], nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("a local image ticket opened a forge attachment: status %d", resp.StatusCode)
		}
		download, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if resp := get(t, f.srv.Addr(), "/attachments/image/att-1"+download[strings.Index(download, "?"):], nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("a download ticket opened a local image: status %d", resp.StatusCode)
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if len(stub.opened) != 0 {
			t.Fatalf("the seam was reached for %v", stub.opened)
		}
	})

	t.Run("content the service no longer holds", func(t *testing.T) {
		f := attachmentFixture(t, &stubTransfer{openErr: errors.New("file changed since it was resolved")})
		if resp := get(t, f.srv.Addr(), mintLocalImage(t, f, "gone"), nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("no seam", func(t *testing.T) {
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AttachmentTransfer = nil })
		if resp := get(t, f.srv.Addr(), mintLocalImage(t, f, "id-1"), nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	if _, err := (&Server{localImageTickets: newTicketBook(4, attachmentTicketTTL)}).MintLocalImageTicket(""); err == nil {
		t.Fatal("MintLocalImageTicket accepted an empty content id")
	}
}

// The literal `image` segment has to win over the download route's
// {threadID} for a path both match, while a two-id path still reaches the
// thread-attachment handler.
func TestLocalImagePatternDoesNotShadowTheDownloadRoute(t *testing.T) {
	stub := &stubTransfer{content: []byte("bytes")}
	f := attachmentFixture(t, stub)

	if resp := get(t, f.srv.Addr(), mintLocalImage(t, f, "content-1"), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("local image route status = %d, want 200", resp.StatusCode)
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
	want := []string{"image/content-1", "3f1b9c2a-thread/8d2e4f6a-att"}
	if fmt.Sprint(stub.opened) != fmt.Sprint(want) {
		t.Fatalf("the seam saw %v, want %v", stub.opened, want)
	}
}

// The preflight answers the shell origin and nobody else, like its siblings.
func TestLocalImagePreflight(t *testing.T) {
	f := attachmentFixture(t, &stubTransfer{content: []byte("x")})
	for origin, want := range map[string]int{ShellOrigin: http.StatusNoContent, "https://evil.example": http.StatusNotFound} {
		req, err := http.NewRequest(http.MethodOptions, "http://"+f.srv.Addr()+"/attachments/image/id-1", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("preflight from %s = %d, want %d", origin, resp.StatusCode, want)
		}
	}
}
