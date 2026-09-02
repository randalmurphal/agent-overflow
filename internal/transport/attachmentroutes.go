package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The attachment byte routes (docs/specs/remote-access.md §4, wave 6b).
//
// Two routes, one shape: a client that already holds a session asks a
// bound RPC for a single-use TICKET naming exactly one transfer, and
// presents it here. The bytes then cross over plain HTTP, streamed, while
// the WebSocket carries nothing but the events it exists for.
//
// What this replaces is the reason it exists. Every attachment byte used
// to ride base64 inside one WS RPC frame, so a 10 MiB image became a
// ~13.4 MB frame on the same socket as the live event stream — the one
// condition the spec forbids outright ("large bodies never block the
// event socket"), and the case conn.go's read-limit comment already said
// belonged on a paged endpoint.
//
// Three properties make these routes safe to reach without a cookie, and
// all three are the ticket's rather than the route's:
//
//   - The ticket is the ADMISSION, exactly as the WS ticket is on the
//     upgrade. It is minted only by a call the per-RPC scope gate already
//     authorized, it is spent by the first presentation, and it lives
//     seconds.
//   - The ticket is SUBJECT-BOUND. A download ticket names one
//     (thread, attachment) pair and a upload ticket names one thread plus
//     the exact byte count and metadata the row will record. Nothing on
//     the request can widen what it admits: the path is compared against
//     the subject rather than read from, and the upload's metadata comes
//     from the subject rather than from a header a caller writes.
//   - There is NO ambient credential in play. Nothing about these
//     requests is authorized by a cookie the browser attaches on its own,
//     so a page at another origin can cause the request but cannot
//     produce the ticket that makes it mean anything. That is why the
//     Origin allow-list guarding /ws and /bootstrap.json is deliberately
//     NOT applied here: it exists to stop a foreign page from spending a
//     credential the browser holds ambiently, and there is none to spend.
//     Applying it anyway would also break the `--connect` stub, whose
//     proxied request legitimately carries the stub's origin rather than
//     the backend's. The Host guard is kept — it costs nothing and keeps
//     one rule across the mux.
//
// One origin can read the answer, and it is the single exception to the
// sentence this header used to end on: the phone shell's
// (shellorigin.go). It has to be, because the shell runs the SAME
// `attachmentTransfer.ts` every other client runs — an RPC mints the
// ticket, the bytes cross on their own connection, and the page reads
// the created row out of the upload's response. Withholding the CORS
// answer there would not withhold anything from anyone else; it would
// only mean the one client that cannot be same-origin is the one client
// that cannot upload. Nothing about the admission changes: the ticket is
// still the whole credential, still single-use, still subject-bound, and
// `Access-Control-Allow-Credentials` is never written, so no browser is
// invited to attach an ambient credential to these routes.
//
// Neither route rides a per-peer rate budget, and that is a decision
// rather than an omission — argued in the internal/surfaces rows.

// AttachmentDownloadPath streams one thread-owned attachment.
//
// The two ids are in the PATH and the ticket is in the query, which is
// the split that makes the request self-describing in a log without the
// log line being a credential: the path says which attachment, the query
// carries the seconds-lived secret. Registered method-qualified so a
// write verb on this pattern is a 405 from the mux rather than something
// the handler has to think about.
const AttachmentDownloadPath = "GET /attachments/{threadID}/{attachmentID}"

// AttachmentDownloadPreflightPath is the same pattern for OPTIONS, and it
// exists only because the pattern above is method-qualified: the mux
// answers an unmatched method with 405, which a browser reads as a
// refused preflight. See shellorigin.go.
const AttachmentDownloadPreflightPath = "OPTIONS /attachments/{threadID}/{attachmentID}"

// AttachmentUploadPath accepts one streamed attachment body.
//
// PUT rather than POST because the ticket already named exactly one
// target: the request creates the resource the ticket describes, and
// presenting the same ticket twice creates nothing. Two segments where
// the download has three, so the two patterns cannot collide however Go's
// mux orders them.
const AttachmentUploadPath = "PUT /attachments/upload"

// AttachmentUploadPreflightPath is that pattern for OPTIONS, for the
// reason its download sibling has one.
const AttachmentUploadPreflightPath = "OPTIONS /attachments/upload"

// AttachmentTicketParam carries a transfer ticket on the request URL.
//
// The same spelling as WSTicketParam and deliberately not the same
// constant: what these two carry is the same IDEA (a single-use,
// subject-bound, seconds-lived admission) drawn from different books, and
// a shared constant would suggest a ticket minted for one could be
// presented at the other. It cannot — see attachmentTickets below.
const AttachmentTicketParam = "ticket"

// attachmentTicketTTL is how long a transfer ticket stays spendable. The
// same 30 seconds a WebSocket ticket gets, and for the same reason: a
// client mints one immediately before the request it is for, so the
// window has to cover a round trip and nothing else.
//
// It deliberately does NOT have to cover the transfer. A ticket is
// consumed when the request ARRIVES, before a byte of a 10 MiB body has
// been read, so a slow upload is bounded by the read window below rather
// than by this.
const attachmentTicketTTL = 30 * time.Second

// maxOutstandingAttachmentTickets bounds each book. A composer send mints
// one per image against a cap of 8, and a lightbox mints one per image in
// the message it opened, so a person cannot reach this; a client that
// mints without ever transferring can, and is what the bound is for.
const maxOutstandingAttachmentTickets = 64

// AttachmentTransferWindow is how long one attachment body may take, in
// either direction. It replaces the server's own HTTPReadTimeout /
// HTTPWriteTimeout for these two routes only, per request.
//
// Exported because the `--connect` stub relays these same bytes through
// its own http.Server, which has the same 60s defaults for the same
// reasons and would otherwise cut a transfer the backend was willing to
// finish. One number, two servers.
//
// The default 60s is right for an RPC and wrong for bytes: 10 MiB inside
// it demands a sustained ~170 KB/s, which a phone on a weak connection
// does not have, and the failure would look like the backend dying rather
// than like a slow upload. Before wave 6b the same bytes rode the
// WebSocket, whose upgrade takes the connection away from net/http so the
// HTTP timeouts never applied to them at all — leaving the defaults in
// place here would therefore have been a NEW way for a large attachment
// to fail. Five minutes puts the floor at ~35 KB/s and still bounds a
// connection that has stopped making progress.
const AttachmentTransferWindow = 5 * time.Minute

// AttachmentContent is one attachment opened for streaming, as the app
// side hands it over.
//
// A ReadSeekCloser rather than bytes, because http.ServeContent needs to
// seek (that is what buys Range and conditional handling for free) and
// because the point of the route is that the payload is never a buffer.
// This package CLOSES it.
type AttachmentContent struct {
	// MimeType is written verbatim as the response Content-Type. The app
	// side guarantees it is one of the image types the attachment store
	// admits, which is what keeps this route from being a way to serve a
	// document at the SPA origin.
	MimeType string
	// ModTime backs Last-Modified and the conditional requests
	// http.ServeContent answers from it.
	ModTime time.Time
	Content io.ReadSeekCloser
}

// AttachmentUpload is one streamed upload as it reaches the app side.
//
// Every field except Body comes from the TICKET's subject, never from the
// request. A filename or a content type read off the wire would be a
// caller describing bytes it is in the middle of sending, and the whole
// value of minting through an authorized RPC is that the description was
// fixed by a call the scope gate had already judged.
type AttachmentUpload struct {
	ThreadID string
	Filename string
	MimeType string
	// Size is the byte count the ticket was minted for. The body must
	// deliver exactly this many.
	Size int64
	// Body is the request body, already capped at Size.
	Body io.Reader
}

// AttachmentTransfer is the narrow app-side surface behind the two byte
// routes. The app satisfies it with an adapter over its attachment store;
// this package never learns what an attachment row is, the same direction
// and the same reason as AuthEndpoints and ScopedTokens.
//
// StoreAttachment answers raw JSON rather than a typed row for the reason
// PasskeyChallenge.Options is raw: the created row's shape belongs to
// internal/store, and a mirror of it here would be a second definition
// that agrees with the first only until somebody adds a field.
type AttachmentTransfer interface {
	// OpenAttachment resolves a thread-owned attachment for reading. The
	// thread-ownership check is the app side's, because it is a property
	// of the stored row.
	OpenAttachment(threadID, attachmentID string) (AttachmentContent, error)
	// StoreAttachment persists one streamed upload and answers the created
	// row.
	StoreAttachment(req AttachmentUpload) (json.RawMessage, error)
}

// attachmentSubjectSeparator joins the fields of a ticket subject. NUL
// because it is the one byte a filename cannot contain on any platform
// this app runs on, which is what lets a filename ride a subject
// unescaped. Minting rejects a name containing one anyway (see
// MintAttachmentUploadTicket): a value that could not round-trip must be
// refused where it enters, not discovered where it is read.
const attachmentSubjectSeparator = "\x00"

// downloadSubject encodes what a download ticket admits.
func downloadSubject(threadID, attachmentID string) string {
	return threadID + attachmentSubjectSeparator + attachmentID
}

// parseDownloadSubject reads it back, refusing anything it did not write.
func parseDownloadSubject(subject string) (threadID, attachmentID string, ok bool) {
	parts := strings.Split(subject, attachmentSubjectSeparator)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// uploadSubject encodes what an upload ticket admits: the thread it lands
// on, the exact byte count, the content type, and the filename.
//
// Filename is LAST and joined with SplitN below, so a name containing the
// separator could at worst truncate itself rather than shift the fields
// that decide where the bytes go.
func uploadSubject(threadID, mimeType, filename string, size int64) string {
	return strings.Join([]string{
		threadID,
		strconv.FormatInt(size, 10),
		mimeType,
		filename,
	}, attachmentSubjectSeparator)
}

// parseUploadSubject reads it back into the shape the app side receives.
func parseUploadSubject(subject string) (AttachmentUpload, bool) {
	parts := strings.SplitN(subject, attachmentSubjectSeparator, 4)
	if len(parts) != 4 {
		return AttachmentUpload{}, false
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || size <= 0 {
		return AttachmentUpload{}, false
	}
	if parts[0] == "" || parts[3] == "" {
		return AttachmentUpload{}, false
	}
	return AttachmentUpload{
		ThreadID: parts[0],
		Size:     size,
		MimeType: parts[2],
		Filename: parts[3],
	}, true
}

// MintAttachmentDownloadTicket returns the relative URL a client fetches
// to read one attachment, ticket included.
//
// The whole URL rather than its parts, so the route's shape has exactly
// one home. A client assembling `/attachments/` + two ids + a query
// parameter would be a second copy of this pattern in a language that
// cannot be checked against the mux.
//
// This mints; it does not authorize. Whether this caller may read this
// attachment was decided by the bound method that called it, which the
// per-RPC scope gate ran first and which re-checked the thread ownership
// itself.
//
// No expiry is returned, and that is the contract rather than an
// oversight: a caller mints immediately before the one request it is for,
// so a deadline it could read would only invite it to hold a ticket it
// should have spent.
func (s *Server) MintAttachmentDownloadTicket(threadID, attachmentID string) (string, error) {
	if threadID == "" || attachmentID == "" {
		return "", errors.New("transport: attachment download ticket needs a thread and an attachment")
	}
	ticket, err := s.attachmentDownloadTickets.mint(downloadSubject(threadID, attachmentID))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/%s/%s?%s=%s",
		url.PathEscape(threadID), url.PathEscape(attachmentID),
		AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// MintAttachmentUploadTicket returns the relative URL a client PUTs one
// attachment body to, ticket included.
//
// Everything the stored row will say about those bytes is fixed HERE and
// travels in the subject. The upload request contributes the bytes and
// nothing else.
func (s *Server) MintAttachmentUploadTicket(threadID, filename, mimeType string, size int64) (string, error) {
	switch {
	case threadID == "" || filename == "":
		return "", errors.New("transport: attachment upload ticket needs a thread and a filename")
	case size <= 0:
		return "", errors.New("transport: attachment upload ticket needs a positive size")
	case strings.Contains(threadID, attachmentSubjectSeparator),
		strings.Contains(filename, attachmentSubjectSeparator),
		strings.Contains(mimeType, attachmentSubjectSeparator):
		// Refused where it enters rather than discovered where it is read:
		// a value that cannot round-trip through the subject must never
		// become a ticket that decodes to something else.
		return "", errors.New("transport: attachment upload metadata contains a reserved byte")
	}
	ticket, err := s.attachmentUploadTickets.mint(uploadSubject(threadID, mimeType, filename, size))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/upload?%s=%s", AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleAttachmentDownload answers AttachmentDownloadPath.
//
// http.ServeContent does the response: Range, If-Modified-Since, If-Range
// and the 206/416 statuses, all from one seekable reader. Hand-rolling
// them would be a second implementation of parsing a header field whose
// edge cases are exactly where a hand-rolled version is wrong.
//
// A ticket is spent per REQUEST, so a client that ranges mints per range.
// That is the honest consequence of single use, and it costs nothing: the
// SPA fetches each image once, whole.
func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.AttachmentTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	subject, ok := s.attachmentDownloadTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok {
		http.NotFound(w, r)
		return
	}
	threadID, attachmentID, ok := parseDownloadSubject(subject)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The path is COMPARED against the subject, never read from. A ticket
	// decides which attachment it admits; the path only has to agree with
	// it, so a request that names a different one is refused rather than
	// quietly served whatever the ticket said.
	if threadID != r.PathValue("threadID") || attachmentID != r.PathValue("attachmentID") {
		http.NotFound(w, r)
		return
	}
	content, err := transfer.OpenAttachment(threadID, attachmentID)
	if err != nil {
		// The ticket was live and its subject was ours, so this is our own
		// bookkeeping failing rather than a caller being refused: log it
		// where an operator can see it, and answer the same 404 a spent
		// ticket gets so the wire discloses no more than it has to.
		log.Printf("transport: open attachment %s: %v", attachmentID, err)
		http.NotFound(w, r)
		return
	}
	defer content.Content.Close()

	extendAttachmentDeadline(w, false)
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	// no-store, not no-cache: the URL that fetched these bytes carried a
	// single-use credential, so a shared cache holding the response would
	// be holding an attachment past the one request that was authorized to
	// read it.
	h.Set("Cache-Control", "no-store")
	// Set before ServeContent, which sniffs only when Content-Type is
	// absent. Sniffing is what nosniff exists to make irrelevant, and the
	// app side has already constrained this to the image allow-list.
	h.Set("Content-Type", content.MimeType)
	// The empty name is deliberate: ServeContent uses it only to guess a
	// content type, which is already set.
	http.ServeContent(w, r, "", content.ModTime, content.Content)
}

// handleAttachmentUpload answers AttachmentUploadPath.
//
// The body streams to the store; nothing here holds it. The cap is
// enforced DURING the read (http.MaxBytesReader), not from Content-Length,
// because a declared length is a claim and the bytes are the verdict — a
// chunked body declares nothing at all.
func (s *Server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.AttachmentTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	subject, ok := s.attachmentUploadTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok {
		http.NotFound(w, r)
		return
	}
	upload, ok := parseUploadSubject(subject)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// A declared length that disagrees with the ticket is refused before
	// anything is read. It saves the transfer, and it is only ever an
	// early exit: a body that lies about its length, or declares none,
	// still fails on the byte count the store checks.
	if r.ContentLength >= 0 && r.ContentLength != upload.Size {
		http.Error(w, "size mismatch", http.StatusBadRequest)
		return
	}
	extendAttachmentDeadline(w, true)
	upload.Body = http.MaxBytesReader(w, r.Body, upload.Size)

	record, err := transfer.StoreAttachment(upload)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "attachment too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Prose does not cross the wire on this listener for a remote
		// caller (§ Credentials and refusal shapes), so the status is the
		// whole message and the detail goes to the log.
		log.Printf("transport: store attachment for thread %s: %v", upload.ThreadID, err)
		http.Error(w, "attachment rejected", http.StatusBadRequest)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	_, _ = w.Write(record)
}

// extendAttachmentDeadline replaces this request's read or write deadline
// with the transfer window. Failing to set one is not fatal: the server's
// own timeout still applies, which is the behavior without this call.
func extendAttachmentDeadline(w http.ResponseWriter, read bool) {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(AttachmentTransferWindow)
	var err error
	if read {
		err = controller.SetReadDeadline(deadline)
	} else {
		err = controller.SetWriteDeadline(deadline)
	}
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("transport: extend attachment transfer deadline: %v", err)
	}
}
