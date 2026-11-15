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
// A bound method on the computer that reads aloud admits one section of a
// reply and mints a ticket naming its speech id; fetching the URL runs the
// synthesis and answers the whole clip as a WAV file. The audio never rides
// a WebSocket frame. Admission is the local-image route's: a single-use
// ticket from a book of its own, bound to the id the path names, under
// /attachments/ so every relay carries it. A reader that no longer wants a
// section hands its URL back (ReleaseSpeechTicket).
//
// A failure reaches a person: the app classifies it as a SpeechError with
// a status and a short message, answered as plain text. A ticket that is
// spent, forged or for another id answers the plain 404 every ticketed
// route does.

// SpeechDownloadPath synthesizes and returns one admitted section.
const SpeechDownloadPath = "GET /attachments/speech/{speechID}"

// SpeechDownloadPreflightPath is the same pattern for OPTIONS, because the
// pattern above is method-qualified and the mux would answer a preflight
// 405.
const SpeechDownloadPreflightPath = "OPTIONS /attachments/speech/{speechID}"

// SpeechTicketTTL is how long a read-aloud ticket, and the section it
// admits, may wait to be fetched.
const SpeechTicketTTL = attachmentTicketTTL

// SpeechTransferWindow bounds one fetch, synthesis and delivery together.
// A clip is a few megabytes at most and holds one of the app's synthesis
// slots until it is delivered, so a reader slower than this is cut off
// rather than given the attachment window.
const SpeechTransferWindow = 2 * time.Minute

// SpeechTransfer is the app-side surface behind the route.
type SpeechTransfer interface {
	// SpeakSentence synthesizes the section speechID admits under ctx,
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
// section's audio from, ticket included. It mints; the bound method that
// admitted the section decided the caller may.
func (s *Server) MintSpeechTicket(speechID string) (string, error) {
	if speechID == "" {
		return "", errors.New("transport: speech ticket needs a speech id")
	}
	ticket, err := s.speechTickets.mint(speechID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s?%s=%s",
		speechURLPrefix, url.PathEscape(speechID), AttachmentTicketParam, url.QueryEscape(ticket)), nil
}

// speechURLPrefix is the path every MintSpeechTicket URL starts with.
const speechURLPrefix = "/attachments/speech/"

// ReleaseSpeechTicket takes back a URL MintSpeechTicket returned whose
// reader no longer wants it: its ticket, if still unspent, is voided, and
// the speech id the URL names is returned for the app to release.
func (s *Server) ReleaseSpeechTicket(ticketURL string) (string, error) {
	parsed, err := url.Parse(ticketURL)
	if err != nil {
		return "", fmt.Errorf("transport: read the speech URL: %w", err)
	}
	speechID, ok := strings.CutPrefix(parsed.Path, speechURLPrefix)
	if !ok || speechID == "" || strings.Contains(speechID, "/") {
		return "", errors.New("transport: not a speech URL")
	}
	if ticket := parsed.Query().Get(AttachmentTicketParam); ticket != "" {
		s.speechTickets.consume(ticket)
	}
	return speechID, nil
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
