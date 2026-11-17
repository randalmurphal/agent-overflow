package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

// A pane page's downloads are the launcher's. WebView2's own download
// dialog opens for every download unless the host takes the download in
// DownloadStarting (Handled), and a download the host took cannot be
// canceled over CDP ("No download item found for the given GUID"), so the
// launcher keeps each one's DownloadOperation, reports its life to the
// backend as ReportDownload, and cancels it on OpCancelDownload
// (download_windows.go). WebView2 saves the file where it would anyway: the
// user's Downloads folder, under a name that does not exist yet.

// The states a DownloadReport carries. Started is the first report of a
// download and completed or interrupted its last; progress comes between.
const (
	DownloadStarted     = "started"
	DownloadProgress    = "progress"
	DownloadCompleted   = "completed"
	DownloadInterrupted = "interrupted"
)

// DownloadProgressInterval is the most often the launcher reports one
// download's bytes. WebView2 raises BytesReceivedChanged per chunk; the
// backend coalesces what reaches the UI further.
const DownloadProgressInterval = 250 * time.Millisecond

// maxDownloadURIBytes bounds the address a report carries, so a data: URL
// cannot push the report past MaxReportDetailBytes.
const maxDownloadURIBytes = 1024

// DownloadReport is the detail of a ReportDownload. ID is the launcher's
// id for the download; Path is the file as Windows names it, final from
// started on; Reason is an interrupted download's cause.
type DownloadReport struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	URI      string `json:"uri,omitempty"`
	Path     string `json:"path,omitempty"`
	Received int64  `json:"received"`
	Total    int64  `json:"total,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Encode answers the report as a report detail. A path too long to carry is
// an error: a cut path would name another file.
func (r DownloadReport) Encode() (string, error) {
	r.URI = truncateUTF8(r.URI, maxDownloadURIBytes)
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(data) > MaxReportDetailBytes {
		return "", fmt.Errorf("download report of %d bytes exceeds %d", len(data), MaxReportDetailBytes)
	}
	return string(data), nil
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
	case DownloadStarted, DownloadProgress, DownloadCompleted, DownloadInterrupted:
	default:
		return DownloadReport{}, fmt.Errorf("download report state %q is unknown", r.State)
	}
	if r.Received < 0 || r.Total < 0 {
		return DownloadReport{}, errors.New("download report has negative bytes")
	}
	if r.State == DownloadCompleted && r.Path == "" {
		return DownloadReport{}, errors.New("a completed download report names no file")
	}
	return r, nil
}

// ValidateDownloadID accepts the page-id class: the id is echoed in a
// directive and reaches log lines.
func ValidateDownloadID(id string) error {
	return validateToken("download id", id, maxPageIDLen)
}

// downloadInterruptReason names COREWEBVIEW2_DOWNLOAD_INTERRUPT_REASON for
// the user.
func downloadInterruptReason(reason uint32) string {
	names := [...]string{
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

// downloadUserCanceled is COREWEBVIEW2_DOWNLOAD_INTERRUPT_REASON_USER_CANCELED.
const downloadUserCanceled = 26

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
