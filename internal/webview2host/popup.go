package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A window a pane page opens (window.open, a target=_blank link or form, a
// middle or modified click) becomes a page of the launcher's, never a
// window of WebView2's own (popup_windows.go). The launcher takes
// NewWindowRequested's deferral, creates a hidden controller in the
// opener's profile, hands its view to the request as the new window and
// completes the deferral; WebView2 then replaces the view's document with
// the new window's, under a new CDP target that names the opener. The
// launcher reads that target and reports the page (ReportPopup) under an id
// it minted, before any other report of the page. The backend decides
// whether it becomes a tab: it attaches to the target and adopts it, or
// closes it with OpClose like any page.

// MaxPendingPopups bounds the new windows whose controller is being created,
// each holding its request's deferral, and so its opener's script, open. A
// request past it is refused: the opener's window.open answers null.
const MaxPendingPopups = 4

// MaxHostPages bounds the controllers the launcher keeps, created and
// popups, the backend's tabs and the popups it has not decided on yet. It
// is the backend's own cap on a process's tabs (browser.maxPagesTotal), so
// a popup refused here is one the backend would refuse.
const MaxHostPages = 64

// MaxPopupURLBytes bounds the address a popup report carries. The backend
// shows it until the page's own reports arrive.
const MaxPopupURLBytes = 2048

// PopupReport is the detail of a ReportPopup: the page id of the page that
// opened the window, the popup's CDP target, and the address it was asked
// to open.
type PopupReport struct {
	Opener string `json:"opener"`
	Target string `json:"target"`
	URL    string `json:"url,omitempty"`
}

// Encode answers the report as a report detail of at most
// MaxReportDetailBytes, its address shortened on a character boundary to
// MaxPopupURLBytes and further while its escaped form does not fit.
func (r PopupReport) Encode() (string, error) {
	// Valid first, so the address the backend decodes is the one bounded.
	r.URL = truncateUTF8(strings.ToValidUTF8(r.URL, "�"), MaxPopupURLBytes)
	if err := r.validate(); err != nil {
		return "", err
	}
	for {
		data, err := encodeReport(r)
		if err != nil {
			return "", err
		}
		over := len(data) - MaxReportDetailBytes
		if over <= 0 {
			return string(data), nil
		}
		// The ids fit with room to spare. A byte of the address is at most
		// six of the report (\u00XX), so this cut never takes more than the
		// excess needs, and each one shortens the address.
		r.URL = truncateUTF8(r.URL, max(len(r.URL)-(over+5)/6, 0))
	}
}

// ParsePopupReport decodes a popup report's detail.
func ParsePopupReport(detail string) (PopupReport, error) {
	var r PopupReport
	if err := json.Unmarshal([]byte(detail), &r); err != nil {
		return PopupReport{}, fmt.Errorf("decode popup report: %w", err)
	}
	if err := r.validate(); err != nil {
		return PopupReport{}, err
	}
	return r, nil
}

func (r PopupReport) validate() error {
	if err := ValidatePageID(r.Opener); err != nil {
		return fmt.Errorf("popup opener: %w", err)
	}
	if err := validateToken("popup target", r.Target, maxPageIDLen); err != nil {
		return err
	}
	if len(r.URL) > MaxPopupURLBytes {
		return errors.New("popup address exceeds its bound")
	}
	return nil
}

// popupPageID is the page id of the launcher's n-th popup. host is a token
// unique to the host; the dash keeps every popup id apart from the
// backend's own page ids, which are hex digits alone.
func popupPageID(host string, n uint64) string {
	return "popup-" + host + "-" + strconv.FormatUint(n, 10)
}
