package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The read-aloud byte route.
//
// A bound method on the computer that reads aloud admits one sentence and
// mints a ticket naming its speech id; fetching the URL runs the synthesis
// and answers the whole clip as a WAV file. The audio never rides a
// WebSocket frame. Admission is the local-image route's: a single-use
// ticket from a book of its own, bound to the id the path names, under
// /attachments/ so every relay carries it.
//
// A failure reaches a person: the app classifies it as a SpeechError with
// a status and a short message, answered as plain text. A ticket that is
// spent, forged or for another id answers the plain 404 every ticketed
// route does.

// SpeechDownloadPath synthesizes and returns one admitted sentence.
const SpeechDownloadPath = "GET /attachments/speech/{speechID}"

// SpeechDownloadPreflightPath is the same pattern for OPTIONS, because the
// pattern above is method-qualified and the mux would answer a preflight
// 405.
const SpeechDownloadPreflightPath = "OPTIONS /attachments/speech/{speechID}"

// SpeechTicketTTL is how long a read-aloud ticket, and the sentence it
// admits, may wait to be fetched.
const SpeechTicketTTL = attachmentTicketTTL

// SpeechTransferWindow bounds one fetch, synthesis and delivery together.
// A clip is a few megabytes at most and holds one of the app's synthesis
// slots until it is delivered, so a reader slower than this is cut off
// rather than given the attachment window.
const SpeechTransferWindow = 2 * time.Minute

// SpeechTransfer is the app-side surface behind the route.
type SpeechTransfer interface {
	// SpeakSentence synthesizes the sentence speechID admits under ctx,
	// which ends when the request does, and returns a WAV file. The route
	// closes it once the response is written or abandoned. A failure a
	// person should see is a *SpeechError.
	SpeakSentence(ctx context.Context, speechID string) (io.ReadSeekCloser, error)
}

// SpeechError is a failed synthesis as the route answers it: Status, with
// Err's text as the body. Err's text must be fit to show.
type SpeechError struct {
	Status int
	Err    error
}

func (e *SpeechError) Error() string { return e.Err.Error() }
func (e *SpeechError) Unwrap() error { return e.Err }

// MintSpeechTicket returns the relative URL a client fetches one admitted
// sentence's audio from, ticket included. It mints; the bound method that
// admitted the sentence decided the caller may.
func (s *Server) MintSpeechTicket(speechID string) (string, error) {
	if speechID == "" {
		return "", errors.New("transport: speech ticket needs a speech id")
	}
	ticket, err := s.speechTickets.mint(speechID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/attachments/speech/%s?%s=%s",
		url.PathEscape(speechID), AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// handleSpeechDownload answers SpeechDownloadPath.
func (s *Server) handleSpeechDownload(w http.ResponseWriter, r *http.Request) {
	transfer := s.cfg.SpeechTransfer
	if transfer == nil {
		http.NotFound(w, r)
		return
	}
	speechID, ok := s.speechTickets.consume(r.URL.Query().Get(AttachmentTicketParam))
	if !ok || speechID == "" || speechID != r.PathValue("speechID") {
		http.NotFound(w, r)
		return
	}
	// The synthesis runs before the first byte; the window covers it and
	// the body.
	extendTransferDeadline(w, SpeechTransferWindow)
	wav, err := transfer.SpeakSentence(r.Context(), speechID)
	if err != nil {
		status, message := http.StatusInternalServerError, err.Error()
		var failure *SpeechError
		if errors.As(err, &failure) && failure.Status >= 400 && failure.Status <= 599 {
			status = failure.Status
		}
		if r.Context().Err() != nil {
			// Nobody is left to read the answer.
			log.Printf("transport: read aloud %s: abandoned by the client: %v", speechID, err)
			return
		}
		log.Printf("transport: read aloud %s: %d %v", speechID, status, err)
		h := w.Header()
		WriteSecurityHeaders(h, s.csp)
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(strings.TrimSpace(message))); err != nil {
			log.Printf("transport: read aloud %s: write the failure: %v", speechID, err)
		}
		return
	}
	defer closeTransferContent(wav, "speech", speechID)
	s.writeTransferContent(w, r, wav, time.Time{}, func(h http.Header) {
		h.Set("Content-Type", "audio/wav")
	})
}
