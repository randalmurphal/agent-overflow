package transport

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// The local-image byte route.
//
// Rendered markdown references images by path on the computer that owns
// the thread (`![x](/abs/shot.png)`). A bound method on that computer gates
// the path, validates the bytes as an image a browser displays, optionally
// derives a display-density version (internal/localimage), and mints a
// ticket naming the opaque content id it holds; the bytes cross here, never
// inside a WebSocket frame.
//
// Everything forgeattachmentroutes.go argues about admission applies
// unchanged: the ticket is the whole credential, single-use and bound to
// one content id the path is compared against, and an id the bounded cache
// expired (or an original whose file changed since it was resolved, or a
// derivative whose cached file was evicted or deleted) answers the same
// 404 a spent ticket does. The Content-Type is the image
// type the backend sniffed from the bytes; nothing that is not an image can
// reach this route.

// LocalImageDownloadPath streams one resolved local image. Under
// /attachments/ for the reason the forge route is: every relay carries that
// subtree. The literal `image` segment is more specific than the download
// route's {threadID}, so the mux prefers this pattern for a path both match.
const LocalImageDownloadPath = "GET /attachments/image/{contentID}"

// LocalImageDownloadPreflightPath is the same pattern for OPTIONS, because
// the pattern above is method-qualified and the mux would answer a
// preflight 405.
const LocalImageDownloadPreflightPath = "OPTIONS /attachments/image/{contentID}"

// LocalImageContent is one resolved local image opened for streaming, as
// the app side hands it over. This package closes Content.
type LocalImageContent struct {
	// MimeType is the image type sniffed from the bytes, written as the
	// response Content-Type.
	MimeType string
	// ModTime backs Last-Modified and the conditional requests
	// http.ServeContent answers.
	ModTime time.Time
	Content io.ReadSeekCloser
}

// MintLocalImageTicket returns the relative URL a client fetches to read one
// resolved local image, ticket included. Same contract as the forge mint:
// it mints, it does not authorize; the bound method that resolved the
// content ran behind the per-RPC scope gate first.
func (s *Server) MintLocalImageTicket(contentID string) (string, error) {
	if contentID == "" {
		return "", errors.New("transport: local image ticket needs a content id")
	}
	ticket, err := s.localImageTickets.mint(contentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/image/%s?%s=%s",
		url.PathEscape(contentID), AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleLocalImageDownload answers LocalImageDownloadPath.
func (s *Server) handleLocalImageDownload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.AttachmentTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	contentID, ok := s.localImageTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok || contentID == "" || contentID != r.PathValue("contentID") {
		http.NotFound(w, r)
		return
	}
	content, err := transfer.OpenLocalImage(contentID)
	if err != nil {
		log.Printf("transport: open local image %s: %v", contentID, err)
		http.NotFound(w, r)
		return
	}
	defer closeTransferContent(content.Content, "local image", contentID)
	s.serveTransferContent(w, r, content.Content, content.ModTime, func(h http.Header) {
		h.Set("Content-Type", content.MimeType)
		if content.MimeType == svgMIME {
			h.Set("Content-Disposition", "attachment")
		}
	})
}

// svgMIME is the one image type that is also a document. Both image routes
// answer it with an attachment disposition: a fetch reads the body either
// way, but a navigation to an unspent URL then downloads it instead of
// rendering a document at the SPA origin.
const svgMIME = "image/svg+xml"

// serveTransferContent streams one ticketed body: a transfer window sized
// from its length, the security headers, no-store, the type headers the
// route writes, and http.ServeContent for Range and conditional requests.
func (s *Server) serveTransferContent(w http.ResponseWriter, r *http.Request, content io.ReadSeeker, modTime time.Time, typeHeaders func(http.Header)) {
	// Sized from the payload rather than taking the floor: a 100 MiB
	// video at the window's minimum sustained rate needs far longer than
	// five minutes, and cutting it would read as the backend dying.
	window := AttachmentTransferWindow
	if size, seekErr := content.Seek(0, io.SeekEnd); seekErr == nil {
		if _, seekErr = content.Seek(0, io.SeekStart); seekErr == nil {
			window = AttachmentTransferWindowFor(size)
		}
	}
	extendTransferDeadline(w, window)

	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	// The URL carried a single-use credential; a shared cache holding the
	// response would hold the bytes past the one request authorized to
	// read them.
	h.Set("Cache-Control", "no-store")
	typeHeaders(h)
	// The empty name is deliberate: ServeContent uses it only to guess a
	// content type, which typeHeaders already set.
	http.ServeContent(w, r, "", modTime, content)
}

// closeTransferContent releases a served body. The response is already
// written, so a close failure can only be logged.
func closeTransferContent(content io.Closer, kind, contentID string) {
	if err := content.Close(); err != nil {
		log.Printf("transport: close %s %s: %v", kind, contentID, err)
	}
}
