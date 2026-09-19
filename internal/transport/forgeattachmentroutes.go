package transport

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The forge-attachment byte route.
//
// A PR/MR body references media the forge holds behind the user's own gh
// / glab login: a GitLab /uploads/<secret>/ image, a GitHub
// user-attachments video. The backend that owns the pull request fetches
// those bytes (internal/forgeattach, internal/git), holds them in a
// bounded cache, and serves them HERE — which is what makes them visible
// on a phone, where no forge login and no VPN to a private instance
// exists.
//
// Everything the sibling routes' header argues applies unchanged: the
// ticket is the whole admission, it is single-use, it is subject-bound,
// and no ambient credential is in play. Two things differ.
//
// The SUBJECT is a content id rather than a pair of row ids. There is no
// thread and no attachment row behind these bytes, only an opaque
// cache-assigned id; the ticket names one, the path is compared against
// it, and an id that expired out of the cache answers the same 404 a
// spent ticket does.
//
// The Content-Type is decided by SIGNATURE and narrowed by KIND. An
// image, video or audio payload goes out under the type its bytes proved
// it to be. Anything else — a PDF, a zip, an unrecognized payload — goes
// out as application/octet-stream with an attachment disposition, no
// matter what its extension suggested, because this route answers at the
// SPA origin and a document that renders there is the one thing the
// posture forbids.

// ForgeAttachmentDownloadPath streams one cached forge attachment.
//
// Under /attachments/ because every relay in the tree — the --connect
// stub, the backend proxy, the phone shell — carries that prefix as one
// subtree; a route outside it would work in the embedded webview and 404
// everywhere else. The literal `forge` segment is more specific than the
// download route's {threadID}, so Go's mux prefers this pattern for a
// path that matches both, and the two coexist without a conflict.
const ForgeAttachmentDownloadPath = "GET /attachments/forge/{contentID}"

// ForgeAttachmentDownloadPreflightPath is the same pattern for OPTIONS,
// for the reason its siblings have one: the pattern above is
// method-qualified, and the mux answers an unmatched method with 405,
// which a browser reads as a refused preflight.
const ForgeAttachmentDownloadPreflightPath = "OPTIONS /attachments/forge/{contentID}"

// ForgeAttachmentContent is one cached forge attachment opened for
// streaming, as the app side hands it over.
type ForgeAttachmentContent struct {
	// MimeType is what the payload's signature said it is. Written as
	// the response Content-Type only for a media Kind.
	MimeType string
	// Kind is "image", "video", "audio" or "file"
	// (internal/forgeattach). It decides the response headers, so the
	// route never has to interpret MimeType.
	Kind string
	// Filename names the download. Used only for a `file`, whose
	// response is a save rather than a render.
	Filename string
	// ModTime is when the bytes were fetched, backing Last-Modified and
	// the conditional requests http.ServeContent answers.
	ModTime time.Time
	Content io.ReadSeekCloser
}

// MintForgeAttachmentTicket returns the relative URL a client fetches to
// read one cached forge attachment, ticket included.
//
// Same contract as MintAttachmentDownloadTicket: this mints, it does not
// authorize. Whether this caller may read this content was decided by
// the bound method that fetched it, which the per-RPC scope gate ran
// first.
func (s *Server) MintForgeAttachmentTicket(contentID string) (string, error) {
	if contentID == "" {
		return "", errors.New("transport: forge attachment ticket needs a content id")
	}
	ticket, err := s.forgeAttachmentTickets.mint(contentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/forge/%s?%s=%s",
		url.PathEscape(contentID), AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleForgeAttachmentDownload answers ForgeAttachmentDownloadPath.
func (s *Server) handleForgeAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.AttachmentTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	contentID, ok := s.forgeAttachmentTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok || contentID == "" {
		http.NotFound(w, r)
		return
	}
	// Compared against the subject, never read from: the ticket decides
	// which content it admits, and the path only has to agree.
	if contentID != r.PathValue("contentID") {
		http.NotFound(w, r)
		return
	}
	content, err := transfer.OpenForgeAttachment(contentID)
	if err != nil {
		// A live ticket whose content is gone is an expiry, not a
		// caller being refused. Same 404 either way so the wire
		// discloses nothing; the log is where the difference lives.
		log.Printf("transport: open forge attachment %s: %v", contentID, err)
		http.NotFound(w, r)
		return
	}
	defer content.Content.Close()

	// Sized from the payload rather than taking the floor: a 100 MiB
	// video at the window's minimum sustained rate needs far longer than
	// five minutes, and cutting it would read as the backend dying.
	window := AttachmentTransferWindow
	if size, seekErr := content.Content.Seek(0, io.SeekEnd); seekErr == nil {
		if _, seekErr = content.Content.Seek(0, io.SeekStart); seekErr == nil {
			window = AttachmentTransferWindowFor(size)
		}
	}
	extendTransferDeadline(w, window)

	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	// The URL carried a single-use credential; a shared cache holding
	// the response would hold the attachment past the one request
	// authorized to read it.
	h.Set("Cache-Control", "no-store")
	switch content.Kind {
	case "image", "video", "audio":
		h.Set("Content-Type", content.MimeType)
	default:
		// NEVER the payload's own type. This is the SPA origin, and the
		// only safe answer for bytes nothing painted is an opaque
		// download — a text/html or image/svg+xml here would be a
		// document executing where the bundle's code runs.
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", attachmentDisposition(content.Filename))
	}
	// The empty name is deliberate: ServeContent uses it only to guess a
	// content type, which is already set.
	http.ServeContent(w, r, "", content.ModTime, content.Content)
}

// attachmentDisposition builds the save-as header for a `file` payload.
//
// Both spellings, because they are read by different clients: the quoted
// ASCII form is what every browser understands and what a client with no
// RFC 5987 support falls back to, and the extended form is the only one
// that can carry a name outside ASCII. Written by hand rather than
// through one FormatMediaType call because that helper emits ONE
// spelling per parameter and the point here is to emit both.
func attachmentDisposition(filename string) string {
	safe := safeDispositionName(filename)
	ascii := asciiFilename(safe)
	header := mime.FormatMediaType("attachment", map[string]string{"filename": ascii})
	if header == "" {
		header = `attachment; filename="download"`
	}
	if ascii != safe && safe != "" {
		header += "; filename*=UTF-8''" + encodeRFC5987(safe)
	}
	return header
}

// safeDispositionName keeps the last path component and drops what a
// header parameter must never carry. The seam's name is already
// constrained, and this route still does not take its word for it: a
// name is a string that crossed from a forge, and the only place its
// shape can be guaranteed is where it is written out.
func safeDispositionName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), " .")
}

// asciiFilename reduces a name to something a quoted header parameter
// can carry on any client: printable ASCII with the quoting and path
// characters removed.
func asciiFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r > 0x7e, r == '"', r == '\\', r == '/', r == ';':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	trimmed := strings.Trim(b.String(), " ._")
	if trimmed == "" {
		return "download"
	}
	return trimmed
}

// encodeRFC5987 percent-encodes everything outside attr-char, which is
// the character set an ext-value may carry unescaped.
func encodeRFC5987(value string) string {
	const unreserved = "!#$&+-.^_`|~"
	var b strings.Builder
	for _, octet := range []byte(value) {
		switch {
		case octet >= 'A' && octet <= 'Z',
			octet >= 'a' && octet <= 'z',
			octet >= '0' && octet <= '9',
			strings.IndexByte(unreserved, octet) >= 0:
			b.WriteByte(octet)
		default:
			fmt.Fprintf(&b, "%%%02X", octet)
		}
	}
	return b.String()
}
