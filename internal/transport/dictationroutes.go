package transport

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The dictation upload route.
//
// A client records speech with MediaRecorder and uploads the recording in
// pieces while it records, so stopping leaves only the last piece to send.
// Each piece is one PUT with its own single-use ticket, minted by a bound
// method on the computer the composer sends to; the ticket names the
// recording, the piece's place in it and its exact length. The bytes never
// ride a WebSocket frame.
//
// Under /attachments/ for the reason the forge and local-image routes are:
// every relay (`--connect`, attached backends, the native shell's URL
// rewrite) already carries that subtree, and a new top-level path would
// need each of them widened.

// DictationUploadPath accepts one piece of a recording. Two segments with a
// literal second one, like the attachment upload, so it cannot collide with
// the three-segment download pattern.
const DictationUploadPath = "PUT /attachments/dictation"

// DictationUploadPreflightPath is that pattern for OPTIONS, because the
// pattern above is method-qualified and the mux would answer a preflight
// 405.
const DictationUploadPreflightPath = "OPTIONS /attachments/dictation"

// DictationChunk is one uploaded piece as it reaches the app side. Every
// field except Body comes from the ticket.
type DictationChunk struct {
	RecordingID string
	Seq         int
	Size        int64
	// Body is the request body, already capped at Size.
	Body io.Reader
}

// DictationTransfer is the app-side surface behind the route.
type DictationTransfer interface {
	// AppendDictationChunk adds one piece to an open recording. A piece out
	// of order, past the recording's size cap, or for a recording that is
	// gone is an error.
	AppendDictationChunk(chunk DictationChunk) error
}

func dictationSubject(recordingID string, seq int, size int64) string {
	return strings.Join([]string{recordingID, strconv.Itoa(seq), strconv.FormatInt(size, 10)}, attachmentSubjectSeparator)
}

func parseDictationSubject(subject string) (DictationChunk, bool) {
	parts := strings.Split(subject, attachmentSubjectSeparator)
	if len(parts) != 3 || parts[0] == "" {
		return DictationChunk{}, false
	}
	seq, err := strconv.Atoi(parts[1])
	if err != nil || seq < 0 {
		return DictationChunk{}, false
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size <= 0 {
		return DictationChunk{}, false
	}
	return DictationChunk{RecordingID: parts[0], Seq: seq, Size: size}, true
}

// MintDictationChunkTicket returns the relative URL a client PUTs one piece
// of a recording to, ticket included. It mints; whether the caller may
// upload to this recording was decided by the bound method that called it.
func (s *Server) MintDictationChunkTicket(recordingID string, seq int, size int64) (string, error) {
	switch {
	case recordingID == "" || strings.Contains(recordingID, attachmentSubjectSeparator):
		return "", errors.New("transport: dictation ticket needs a recording id")
	case seq < 0:
		return "", errors.New("transport: dictation ticket needs a piece number")
	case size <= 0:
		return "", errors.New("transport: dictation ticket needs a positive size")
	}
	ticket, err := s.dictationTickets.mint(dictationSubject(recordingID, seq, size))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/dictation?%s=%s", AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleDictationUpload answers DictationUploadPath with 204 once the piece
// is part of its recording.
func (s *Server) handleDictationUpload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.DictationTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	subject, ok := s.dictationTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok {
		http.NotFound(w, r)
		return
	}
	chunk, ok := parseDictationSubject(subject)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != chunk.Size {
		http.Error(w, "size mismatch", http.StatusBadRequest)
		return
	}
	extendTransferDeadline(w, AttachmentTransferWindowFor(chunk.Size))
	chunk.Body = http.MaxBytesReader(w, r.Body, chunk.Size)
	if err := transfer.AppendDictationChunk(chunk); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "recording piece too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("transport: append dictation piece %d to %s: %v", chunk.Seq, chunk.RecordingID, err)
		http.Error(w, "recording piece rejected", http.StatusBadRequest)
		return
	}
	WriteSecurityHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
