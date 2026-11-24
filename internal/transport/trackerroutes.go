package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
)

// The issue tracker byte routes: a file attached to a ticket and an
// attachment of one saved. Each request carries a single-use ticket a bound
// method minted on the computer that holds the tracker connection, naming
// the connection, the issue and the one file; the bytes stream between the
// client and the tracker through this computer without being held here.
//
// Under /attachments/ for the reason the forge, local-image and dictation
// routes are: every relay already carries that subtree. The literal
// `tracker` segment is more specific than the thread download's
// {threadID}, so the mux prefers these patterns for a path both match.

// TrackerUploadPath accepts one file to attach to a ticket.
const TrackerUploadPath = "PUT /attachments/tracker/upload"

// TrackerUploadPreflightPath is that pattern for OPTIONS, because the
// pattern above is method-qualified and the mux would answer a preflight
// 405.
const TrackerUploadPreflightPath = "OPTIONS /attachments/tracker/upload"

// TrackerDownloadPath streams one attachment of a ticket.
const TrackerDownloadPath = "GET /attachments/tracker/{attachmentID}"

// TrackerDownloadPreflightPath is that pattern for OPTIONS.
const TrackerDownloadPreflightPath = "OPTIONS /attachments/tracker/{attachmentID}"

// TrackerUpload is one file to attach as it reaches the app side. Every
// field but Context and Body comes from the ticket.
type TrackerUpload struct {
	// Context is the request's: its values name the caller, and it ends
	// when the client leaves.
	Context    context.Context
	Connection string
	Issue      string
	Filename   string
	// Size is the byte count the ticket was minted for.
	Size int64
	// Body is the request body, capped at Size. A body that ends short of
	// Size fails its read. Close ends a read of it in progress and every
	// later one, which is how the write stops reading a client that
	// stopped sending (trackerapi.Part).
	Body io.ReadCloser
}

// TrackerTransfer is the app-side surface behind the tracker byte routes.
// A failure is answered as writeTrackerTransferError says.
type TrackerTransfer interface {
	// AttachTrackerFile attaches one streamed file to an issue and
	// answers the write's outcome as JSON. A write the tracker judged,
	// whatever it judged, is an answer and not an error.
	AttachTrackerFile(upload TrackerUpload) (json.RawMessage, error)
	// DownloadTrackerAttachment streams one attachment of an issue to to,
	// ending when ctx does.
	DownloadTrackerAttachment(ctx context.Context, connection, issue, attachment string, to io.Writer) error
}

// trackerUploadSubject encodes what an upload ticket admits. The filename
// is last and read back with SplitN, as the thread upload's is.
func trackerUploadSubject(connection, issue, filename string, size int64) string {
	return strings.Join([]string{connection, issue, strconv.FormatInt(size, 10), filename}, attachmentSubjectSeparator)
}

func parseTrackerUploadSubject(subject string) (TrackerUpload, bool) {
	parts := strings.SplitN(subject, attachmentSubjectSeparator, 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[3] == "" {
		return TrackerUpload{}, false
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size < 0 {
		return TrackerUpload{}, false
	}
	return TrackerUpload{Connection: parts[0], Issue: parts[1], Size: size, Filename: parts[3]}, true
}

// trackerDownload is what a download ticket admits.
type trackerDownload struct {
	connection, issue, attachment, filename string
}

func (d trackerDownload) subject() string {
	return strings.Join([]string{d.connection, d.issue, d.attachment, d.filename}, attachmentSubjectSeparator)
}

func parseTrackerDownloadSubject(subject string) (trackerDownload, bool) {
	parts := strings.SplitN(subject, attachmentSubjectSeparator, 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return trackerDownload{}, false
	}
	return trackerDownload{connection: parts[0], issue: parts[1], attachment: parts[2], filename: parts[3]}, true
}

// subjectFieldsOK reports whether every field can ride a subject: none
// empty, none holding the separator.
func subjectFieldsOK(fields ...string) bool {
	for _, f := range fields {
		if f == "" || strings.Contains(f, attachmentSubjectSeparator) {
			return false
		}
	}
	return true
}

// MintTrackerUploadTicket returns the relative URL a client PUTs one file
// of size bytes to, attaching it to issue on connection as filename. It
// mints; whether the caller may attach was decided by the bound method
// that called it.
func (s *Server) MintTrackerUploadTicket(connection, issue, filename string, size int64) (string, error) {
	switch {
	case !subjectFieldsOK(connection, issue, filename):
		return "", errors.New("transport: tracker upload ticket needs a connection, an issue and a filename, none holding a NUL byte")
	case size < 0:
		return "", errors.New("transport: tracker upload ticket needs a size that is not negative")
	}
	ticket, err := s.trackerUploadTickets.mint(trackerUploadSubject(connection, issue, filename, size))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/tracker/upload?%s=%s", AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// MintTrackerDownloadTicket returns the relative URL a client GETs one
// attachment of issue on connection from, saved as filename.
func (s *Server) MintTrackerDownloadTicket(connection, issue, attachment, filename string) (string, error) {
	d := trackerDownload{connection: connection, issue: issue, attachment: attachment, filename: filename}
	if !subjectFieldsOK(connection, issue, attachment) || strings.Contains(filename, attachmentSubjectSeparator) {
		return "", errors.New("transport: tracker download ticket needs a connection, an issue and an attachment, none holding a NUL byte")
	}
	ticket, err := s.trackerDownloadTickets.mint(d.subject())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/tracker/%s?%s=%s", url.PathEscape(attachment), AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleTrackerUpload answers TrackerUploadPath with the write's outcome.
func (s *Server) handleTrackerUpload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.TrackerTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	subject, ok := s.trackerUploadTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok {
		http.NotFound(w, r)
		return
	}
	upload, ok := parseTrackerUploadSubject(subject)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// A declared length that disagrees with the ticket is refused before
	// anything reaches the tracker. A body that declares none and runs
	// short or long fails as it is sent, and the write is not applied.
	if r.ContentLength >= 0 && r.ContentLength != upload.Size {
		s.writeTrackerTransferError(w, "attach a file to "+upload.Issue, errorsx.Public("tracker_upload_size_changed",
			fmt.Sprintf("the file is now %d bytes, not the %d it was when its upload was prepared; select the file again and attach it", r.ContentLength, upload.Size),
			fmt.Errorf("transport: an upload ticketed for %d bytes declared %d", upload.Size, r.ContentLength)))
		return
	}
	// The bytes get the transfer window for their size; the answer, which
	// comes after the tracker's answer and the refresh, the whole upload's
	// window (UploadWindow).
	window := s.trackerUploadWindow(upload.Size)
	extendTransferDeadline(w, window)
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(UploadWindow(window))); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("transport: extend the attach's answer deadline: %v", err)
	}
	upload.Body = &trackerUploadBody{Reader: http.MaxBytesReader(w, r.Body, upload.Size), rc: rc}
	upload.Context = r.Context()
	view, err := transfer.AttachTrackerFile(upload)
	if err != nil {
		s.writeTrackerTransferError(w, "attach a file to "+upload.Issue, err)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	_, _ = w.Write(view)
}

// trackerUploadBody is an upload's request body whose Close moves the
// request's read deadline to now, once: net/http gives a handler no other
// way to end a read of its body in progress. The answer is still written.
type trackerUploadBody struct {
	io.Reader
	rc   *http.ResponseController
	once sync.Once
	err  error
}

func (b *trackerUploadBody) Close() error {
	b.once.Do(func() {
		if err := b.rc.SetReadDeadline(time.Now()); err != nil {
			b.err = fmt.Errorf("transport: stop reading an upload: %w", err)
		}
	})
	return b.err
}

// handleTrackerDownload answers TrackerDownloadPath, bounded by its
// progress (DownloadByProgress), not by a total. The status and
// headers wait for the attachment's first byte, so a failure before it
// (a connection turned off, an attachment the tracker no longer has) is
// answered with its sentence. The length is not declared: nothing this
// computer holds says how many bytes the tracker will send. A failure
// after the first byte ends the response without its final chunk, which
// every client reads as a failed transfer.
func (s *Server) handleTrackerDownload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.TrackerTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	subject, ok := s.trackerDownloadTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, ok := parseTrackerDownloadSubject(subject)
	if !ok || d.attachment != r.PathValue("attachmentID") {
		http.NotFound(w, r)
		return
	}
	progress, err := DownloadByProgress(w, r, s.trackerStall)
	if err != nil {
		// Closed after the answer: net/http would otherwise read the body
		// before answering, and its client may withhold it.
		w.Header().Set("Connection", "close")
		s.writeTrackerTransferError(w, "download attachment "+d.attachment+" of "+d.issue,
			errorsx.Public("tracker_download_has_body", "a download sends no body; ask for the attachment again without one", err))
		return
	}
	w = progress
	held := &heldResponse{w: w, begin: func(h http.Header) {
		WriteSecurityHeaders(h, s.csp)
		h.Set("Cache-Control", "no-store")
		// Never the file's own type: these bytes are a save, and at the
		// SPA origin a document that rendered would run beside the bundle.
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", attachmentDisposition(d.filename))
	}}
	err = transfer.DownloadTrackerAttachment(r.Context(), d.connection, d.issue, d.attachment, held)
	switch {
	case err == nil:
		held.start()
	case !held.started:
		s.writeTrackerTransferError(w, "download attachment "+d.attachment+" of "+d.issue, err)
	default:
		log.Printf("transport: download attachment %s of %s ended after its first byte: %v", d.attachment, d.issue, err)
		panic(http.ErrAbortHandler)
	}
}

// heldResponse writes no status or header until its first byte.
type heldResponse struct {
	w       http.ResponseWriter
	begin   func(http.Header)
	started bool
}

func (h *heldResponse) start() {
	if !h.started {
		h.started = true
		h.begin(h.w.Header())
		h.w.WriteHeader(http.StatusOK)
	}
}

func (h *heldResponse) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	h.start()
	return h.w.Write(p)
}

// writeTrackerTransferError answers a tracker transfer's failure as a
// method's failure is answered (CallerError): 400 for a refusal the app
// worded for the person, 500 and a log line for any other.
func (s *Server) writeTrackerTransferError(w http.ResponseWriter, doing string, err error) {
	answer := CallerError(err)
	status := http.StatusBadRequest
	if _, _, public := errorsx.PublicDetails(err); !public {
		log.Printf("transport: %s: %v", doing, err)
		status = http.StatusInternalServerError
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(trackerTransferError{Code: answer.Code, Message: answer.Message})
}

// trackerTransferError is a tracker transfer's failure body.
type trackerTransferError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
