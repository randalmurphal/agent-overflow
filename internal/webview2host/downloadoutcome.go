package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The typed outcomes of a launcher download: what one read of its
// operation found (downloadObservation), what the launcher does with that
// read (downloadVerdict), and what came of the backend's last cancel
// (CancelOutcome). They are cross-platform so their rules are unit-tested
// off Windows; download_reconcile_windows.go only reads the operation and
// carries a verdict out.

// CancelAttempt numbers the backend's cancels of one download, from 1, in
// the order the backend sent them. Zero is no attempt.
type CancelAttempt uint64

// CancelOutcome is what came of a download's last cancel attempt the
// launcher took: WebView2 took it, or refused it and why. The zero value is
// no attempt. Only CancelTaken, CancelRefused and the wire decoder make
// one, so a refusal always names its attempt.
type CancelOutcome struct {
	attempt CancelAttempt
	refusal string
}

// CancelTaken is attempt, which WebView2 took. attempt is not 0.
func CancelTaken(attempt CancelAttempt) CancelOutcome {
	mustAttempt(attempt)
	return CancelOutcome{attempt: attempt}
}

// CancelRefused is attempt, which WebView2 refused with err; its refusal
// begins with DownloadCancelRefusedReason. attempt is not 0.
func CancelRefused(attempt CancelAttempt, err error) CancelOutcome {
	mustAttempt(attempt)
	return CancelOutcome{attempt: attempt, refusal: fmt.Sprintf("%s: %v", DownloadCancelRefusedReason, err)}
}

func mustAttempt(attempt CancelAttempt) {
	if attempt == 0 {
		panic("webview2host: a cancel outcome of no attempt")
	}
}

// Attempt is the attempt the outcome is of, 0 for none.
func (o CancelOutcome) Attempt() CancelAttempt { return o.attempt }

// Refusal answers why WebView2 refused the attempt, and false when it took
// it or there was none.
func (o CancelOutcome) Refusal() (string, bool) { return o.refusal, o.refusal != "" }

// IsZero reports no attempt, for the report's omitzero.
func (o CancelOutcome) IsZero() bool { return o == CancelOutcome{} }

type cancelOutcomeWire struct {
	Attempt CancelAttempt `json:"attempt"`
	Refusal string        `json:"refusal,omitempty"`
}

func (o CancelOutcome) MarshalJSON() ([]byte, error) {
	return json.Marshal(cancelOutcomeWire{Attempt: o.attempt, Refusal: o.refusal})
}

func (o *CancelOutcome) UnmarshalJSON(data []byte) error {
	var wire cancelOutcomeWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Attempt == 0 {
		return errors.New("a cancel outcome names no attempt")
	}
	*o = CancelOutcome{attempt: wire.Attempt, refusal: wire.Refusal}
	return nil
}

// downloadSample is why a download's operation is read.
type downloadSample int

const (
	// sampleChange: WebView2 raised a state change, or the download just
	// started or resumed.
	sampleChange downloadSample = iota + 1
	// sampleStatus: a directive naming the download, or its page asking to
	// close, needs to know where it is.
	sampleStatus
	// sampleCancelBound: the bound of the backend's cancel passed.
	sampleCancelBound
	// sampleRetire: the download's unannounced popup goes, and its teardown
	// would cancel what still runs.
	sampleRetire
)

// observationKind is what a read of a download's operation found.
type observationKind int

const (
	observedRunning observationKind = iota + 1
	observedCompleted
	observedInterrupted
	observedUnread
)

// downloadObservation is one read of a download's operation: running,
// completed with the file it read, interrupted with the reason it read, or
// unread with why. Only its constructors make one.
type downloadObservation struct {
	kind   observationKind
	path   string
	reason uint32
	unread error
}

func observeRunning() downloadObservation { return downloadObservation{kind: observedRunning} }

// observeCompleted is a completed operation whose file reads path, "" when
// it could not be read.
func observeCompleted(path string) downloadObservation {
	return downloadObservation{kind: observedCompleted, path: path}
}

// observeInterrupted is an interrupted operation; an interruption whose
// cause could not be read is reason 0, "the download was interrupted".
func observeInterrupted(reason uint32) downloadObservation {
	return downloadObservation{kind: observedInterrupted, reason: reason}
}

func observeUnread(err error) downloadObservation {
	return downloadObservation{kind: observedUnread, unread: err}
}

// verdictKind is what the launcher does with a download after a read.
type verdictKind int

const (
	// verdictKeep leaves the download running, to be read again.
	verdictKeep verdictKind = iota + 1
	// verdictWaitAnotherBound leaves it running and waits one more cancel
	// bound.
	verdictWaitAnotherBound
	// verdictEnd ends it with a terminal snapshot.
	verdictEnd
)

// downloadVerdict is what reconcileDownload does: keep the download, wait
// another bound, or end it in state with reason, first setting its file to
// path when one was read and canceling its operation when cancelFirst.
type downloadVerdict struct {
	kind        verdictKind
	state       string
	reason      string
	path        string
	cancelFirst bool
}

// verdict decides a download from one read and why it was read. Only a
// read that found the operation completed or interrupted ends it as
// WebView2 ended it. A download still running or unread ends only for an
// explicit cause, never as WebView2's: at the cancel bound a running one
// ends with downloadCancelBoundReason (WebView2 did not end it, so the
// launcher stops following it) and an unread one waits another bound; at
// retirement either is canceled and ends with PopupGoneReason. An
// interrupted operation other than by the user's cancel is canceled first,
// so nothing WebView2 might continue on it runs unobserved once it is let
// go; on the real runtime that cancel succeeds and leaves its state and
// reason.
func (seen downloadObservation) verdict(why downloadSample) downloadVerdict {
	switch seen.kind {
	case observedCompleted:
		return downloadVerdict{kind: verdictEnd, state: DownloadCompleted, path: seen.path}
	case observedInterrupted:
		return downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, reason: downloadInterruptReason(seen.reason), cancelFirst: seen.reason != downloadUserCanceled}
	case observedRunning, observedUnread:
		switch why {
		case sampleChange, sampleStatus:
			return downloadVerdict{kind: verdictKeep}
		case sampleCancelBound:
			if seen.kind == observedUnread {
				return downloadVerdict{kind: verdictWaitAnotherBound}
			}
			return downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, reason: downloadCancelBoundReason}
		case sampleRetire:
			return downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, reason: PopupGoneReason, cancelFirst: true}
		}
		panic(fmt.Sprintf("webview2host: a download read for an unknown cause %d", why))
	}
	panic(fmt.Sprintf("webview2host: a download observation of unknown kind %d", seen.kind))
}

// downloadCancelBoundReason is the cause of the interrupted snapshot that
// ends a canceled download WebView2 still reads as running at the bound.
const downloadCancelBoundReason = "the browser did not end the download after its cancel, so it is no longer followed"
