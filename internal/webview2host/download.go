package webview2host

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A pane page's downloads are the launcher's. WebView2's own download
// dialog opens for every download unless the host takes the download in
// DownloadStarting (Handled), and a download the host took cannot be
// canceled over CDP ("No download item found for the given GUID"), so the
// launcher keeps each one's DownloadOperation, reports it to the backend as
// ReportDownload, and answers OpCancelDownload and OpDownloadStatus
// (download_windows.go). WebView2 saves the file where it would anyway: the
// user's Downloads folder, under a name that does not exist yet.
//
// Every report is the download's whole snapshot, numbered (Seq) in the
// order the launcher took it, and the backend applies a download's
// snapshots one at a time, each only if it is newer than the last it
// applied: one for a download it has no row for creates the row, a repeat
// or an older one changes nothing, and nothing after a terminal snapshot
// reopens it. So a lost report heals on the next one, and reports that
// overtake each other on the way settle as the launcher took them. The
// launcher answers every directive naming a download with a snapshot
// marked as an answer (Answer), which settles or advances a row and never
// creates one: a running download's, an ended one's terminal snapshot
// again (kept while its page lives, up to MaxEndedDownloads per page), or,
// for an id it has no record of, DownloadUnknown.
//
// The backend numbers its cancel attempts of a download (Directive.Cancel),
// and every snapshot after one carries the outcome of the last the launcher
// took (Cancel, a CancelOutcome). So a lost answer to a cancel heals on the
// download's next snapshot, a status answer included, and a status naming
// an attempt the launcher never got makes it.

// The states a DownloadReport carries: running until one terminal state.
// DownloadUnknown is no download's state: it answers a directive naming an
// id the launcher has no record of, and ends only a row still running.
const (
	DownloadRunning     = "running"
	DownloadCompleted   = "completed"
	DownloadInterrupted = "interrupted"
	DownloadUnknown     = "unknown"
)

// DownloadProgressInterval is the most often the launcher reports one
// download's bytes. WebView2 raises BytesReceivedChanged per chunk; the
// backend coalesces what reaches the UI further.
const DownloadProgressInterval = 250 * time.Millisecond

// MaxEndedDownloads bounds the terminal snapshots the launcher keeps per
// page to answer a directive about an ended download; the oldest goes
// first, and a directive naming it is answered as an unknown id is.
const MaxEndedDownloads = 64

// DownloadUnknownReason is the reason a DownloadUnknown answer carries.
const DownloadUnknownReason = "the browser host has no record of this download"

// DownloadCancelRefusedReason begins a refused CancelOutcome's refusal:
// WebView2 refused the download's last cancel, the download runs on, and
// its cancel is no longer requested.
const DownloadCancelRefusedReason = "the browser refused to cancel the download"

// DownloadPathTooLongReason is a completed snapshot's reason when the
// file's path does not fit a report: the file is saved, and the backend
// cannot name it.
const DownloadPathTooLongReason = "the saved file's path is too long to report, so it is not opened from here"

// DownloadReport is the detail of a ReportDownload: the download's whole
// snapshot. ID is the launcher's id for the download; Seq the snapshot's
// number among the download's, from 1, increasing in the order the
// launcher took them (0 for DownloadUnknown, which is no snapshot); Answer
// marks one sent in answer to a directive; Name the saved file's name and
// Path the file as Windows names it, from the first snapshot on; URI its
// address, shortened or left out so the report fits; Reason an interrupted
// download's cause, why a completed one's file is not reported
// (DownloadPathTooLongReason), or DownloadUnknownReason; Cancel what came
// of the backend's last cancel attempt the launcher took.
type DownloadReport struct {
	ID       string        `json:"id"`
	Seq      uint64        `json:"seq,omitempty"`
	Answer   bool          `json:"answer,omitempty"`
	State    string        `json:"state"`
	URI      string        `json:"uri,omitempty"`
	Name     string        `json:"name,omitempty"`
	Path     string        `json:"path,omitempty"`
	Received int64         `json:"received"`
	Total    int64         `json:"total,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Cancel   CancelOutcome `json:"cancel,omitzero"`
}

// maxDownloadNameBytes bounds the name a report carries; Windows bounds a
// file name to 255 UTF-16 units.
const maxDownloadNameBytes = 1024

// MaxDownloadURIBytes bounds the address the launcher keeps for a download
// and reports with each of its snapshots.
const MaxDownloadURIBytes = 2048

// downloadAddress is a download's address as the launcher keeps it, taken
// once as the download starts: at most MaxDownloadURIBytes of it, copied,
// for its reports and its kept terminal snapshot, so an address of any
// length (a data URL carries its whole file) is neither retained nor
// encoded again on every report; and a digest of the whole address, which
// a resume is matched by (resumedDownload). An address the runtime did not
// answer is empty and matches nothing: two unread addresses are not one.
type downloadAddress struct {
	shown  string
	digest [sha256.Size]byte
	known  bool
}

func newDownloadAddress(uri string) downloadAddress {
	if uri == "" {
		return downloadAddress{}
	}
	shown := truncateUTF8(uri, MaxDownloadURIBytes)
	if len(shown) < len(uri) {
		shown = strings.Clone(shown)
	}
	return downloadAddress{shown: shown, digest: sha256.Sum256([]byte(uri)), known: true}
}

// same reports whether two downloads are from one address, both known.
func (a downloadAddress) same(b downloadAddress) bool {
	return a.known && b.known && a.digest == b.digest
}

// Encode answers the report as a report detail of at most
// MaxReportDetailBytes. The optional address is shortened, on a character
// boundary, or left out until the report fits. A path that cannot fit is
// left out, never cut, since a cut path names another file: a running
// snapshot then carries the name alone, and a completed one says why it
// names no file (DownloadPathTooLongReason).
func (r DownloadReport) Encode() (string, error) {
	r.Name = truncateUTF8(r.Name, maxDownloadNameBytes)
	for {
		data, err := encodeReport(r)
		if err != nil {
			return "", err
		}
		over := len(data) - MaxReportDetailBytes
		switch {
		case over <= 0:
			return string(data), nil
		case r.URI != "":
			// Each byte of the address is at least one byte of the report,
			// so this cut alone brings the report within the bound unless
			// the rest is too long anyway.
			r.URI = truncateUTF8(r.URI, max(len(r.URI)-over, 0))
		case r.Path != "":
			r.Path = ""
			if r.State == DownloadCompleted && r.Reason == "" {
				r.Reason = DownloadPathTooLongReason
			}
		default:
			return "", fmt.Errorf("download report of %d bytes exceeds %d", len(data), MaxReportDetailBytes)
		}
	}
}

// encodeReport is a report's JSON without HTML escaping, which would
// grow every & < > in an address sixfold.
func encodeReport(r any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ParseDownloadReport decodes a download report's detail.
func ParseDownloadReport(detail string) (DownloadReport, error) {
	var r DownloadReport
	if err := json.Unmarshal([]byte(detail), &r); err != nil {
		return DownloadReport{}, fmt.Errorf("decode download report: %w", err)
	}
	if err := ValidateDownloadID(r.ID); err != nil {
		return DownloadReport{}, err
	}
	switch r.State {
	case DownloadRunning, DownloadCompleted, DownloadInterrupted:
		if r.Seq == 0 {
			return DownloadReport{}, errors.New("a download snapshot has no number")
		}
	case DownloadUnknown:
		if r.Seq != 0 || !r.Answer {
			return DownloadReport{}, errors.New("an unknown-download report must be an unnumbered answer")
		}
	default:
		return DownloadReport{}, fmt.Errorf("download report state %q is unknown", r.State)
	}
	if r.Received < 0 || r.Total < 0 {
		return DownloadReport{}, errors.New("download report has negative bytes")
	}
	if r.State == DownloadRunning && r.Reason != "" {
		return DownloadReport{}, errors.New("a running download report carries no reason")
	}
	if r.State == DownloadCompleted && r.Path == "" && r.Reason == "" {
		return DownloadReport{}, errors.New("a completed download report names no file and says no reason")
	}
	return r, nil
}

// Terminal reports whether the snapshot is the download's last state.
func (r DownloadReport) Terminal() bool {
	return r.State == DownloadCompleted || r.State == DownloadInterrupted
}

// ValidateDownloadID accepts the page-id class: the id is echoed in a
// directive and reaches log lines.
func ValidateDownloadID(id string) error {
	return validateToken("download id", id, maxPageIDLen)
}

// COREWEBVIEW2_DOWNLOAD_INTERRUPT_REASON values the launcher names.
const (
	downloadUserCanceled = 26
	downloadReasonCount  = 30
)

// downloadInterruptReason names COREWEBVIEW2_DOWNLOAD_INTERRUPT_REASON for
// the user.
func downloadInterruptReason(reason uint32) string {
	names := [downloadReasonCount]string{
		"the download was interrupted",
		"the file could not be written",
		"access to the file was denied",
		"the disk is full",
		"the file name is too long",
		"the file is too large for the disk",
		"the file was flagged as malicious",
		"a temporary file error stopped the download",
		"a policy blocked the download",
		"the file failed a security check",
		"the file was shorter than expected",
		"the file did not match its expected hash",
		"the network failed",
		"the network timed out",
		"the network disconnected",
		"the server went down",
		"the network request was invalid",
		"the server failed",
		"the server does not support resuming",
		"the server sent bad content",
		"the server refused access",
		"the server's certificate was refused",
		"the server forbade the download",
		"the server sent an unexpected response",
		"the server's response was a different length than it announced",
		"the server redirected across origins",
		"the download was canceled",
		"the browser shut down",
		"the download was paused",
		"the download process crashed",
	}
	if int(reason) < len(names) {
		return names[reason]
	}
	return "the download was interrupted (reason " + strconv.FormatUint(uint64(reason), 10) + ")"
}

// progressGate passes one progress report per DownloadProgressInterval.
type progressGate struct {
	last time.Time
}

// due reports whether a progress report may go now, and if so starts the
// next interval.
func (g *progressGate) due(now time.Time) bool {
	if !g.last.IsZero() && now.Sub(g.last) < DownloadProgressInterval {
		return false
	}
	g.last = now
	return true
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// endedDownloads is a page's terminal snapshots, kept to answer a
// directive about an ended download, at most MaxEndedDownloads, oldest
// first out.
type endedDownloads struct {
	byID  map[string]DownloadReport
	order []string
}

// add keeps a terminal snapshot.
func (e *endedDownloads) add(r DownloadReport) {
	if e.byID == nil {
		e.byID = make(map[string]DownloadReport)
	}
	if _, kept := e.byID[r.ID]; !kept {
		e.order = append(e.order, r.ID)
	}
	e.byID[r.ID] = r
	for len(e.order) > MaxEndedDownloads {
		delete(e.byID, e.order[0])
		e.order = e.order[1:]
	}
}

// answer is what a directive naming an ended or unknown id gets: the ended
// download's terminal snapshot, under its own number, or DownloadUnknown.
func (e *endedDownloads) answer(id string) DownloadReport {
	if r, ok := e.byID[id]; ok {
		r.Answer = true
		return r
	}
	return DownloadReport{ID: id, State: DownloadUnknown, Reason: DownloadUnknownReason, Answer: true}
}
