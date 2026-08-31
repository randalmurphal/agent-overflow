package webview2host

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Op is the closed set of pane-host commands the backend may send. An op
// outside this set is dropped by the launcher with a log line and never
// guessed at: every one of these moves, shows or destroys a real browser
// window, so a near-miss spelling must fail loudly rather than land on
// whichever op looks closest.
type Op string

const (
	// OpCreate creates a hidden controller for PageID on ProfileID.
	OpCreate Op = "create"
	// OpBounds positions the controller inside the launcher window.
	// X/Y/W/H are DIPs in the host window's client coordinates.
	OpBounds Op = "bounds"
	// OpShow makes the controller visible and raises it to the top of the
	// host's child z-order.
	OpShow Op = "show"
	// OpHide makes the controller invisible without destroying it. A
	// hidden controller keeps its page, and stays drivable over CDP.
	OpHide Op = "hide"
	// OpClose destroys the controller.
	OpClose Op = "close"
	// OpDevTools opens the Chromium DevTools window for the page.
	OpDevTools Op = "devtools"
	// OpCloseProfile closes every controller on ProfileID.
	OpCloseProfile Op = "close-profile"
)

// Directive is one pane-host command. It is the JSON payload of an
// eventchan.BrowserHost frame.
//
// The zero value is not a valid directive: Validate rejects it, and the
// launcher validates BEFORE dispatching, so no field below is ever
// consumed unchecked.
type Directive struct {
	Op        Op      `json:"op"`
	PageID    string  `json:"pageId,omitempty"`
	ProfileID string  `json:"profileId,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	W         float64 `json:"w,omitempty"`
	H         float64 `json:"h,omitempty"`
	// Ephemeral asks for an InPrivate profile on create, and on
	// close-profile says the profile directory is expected to be
	// discarded. It is meaningless on every other op.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// maxPaneDIP bounds every coordinate a bounds directive may carry. It is
// far above any real display arrangement and far below the point where
// float64 -> int32 conversion for RECT would wrap, which is the actual
// hazard: a NaN or 1e30 reaching PutBounds is undefined behaviour in the
// COM layer, not a visibly wrong rectangle.
const maxPaneDIP = 100000.0

// ErrUnknownOp is what an unrecognised op validates to. The launcher
// logs and drops on this rather than tearing down the bridge.
var ErrUnknownOp = errors.New("unknown browser host op")

// Validate enforces the whole contract. It is the trust boundary: the
// directive names a profile that becomes a directory on disk and a page
// the host will create OS windows for.
func (d Directive) Validate() error {
	switch d.Op {
	case OpCreate:
		if err := ValidatePageID(d.PageID); err != nil {
			return err
		}
		return ValidateProfileID(d.ProfileID)
	case OpBounds:
		if err := ValidatePageID(d.PageID); err != nil {
			return err
		}
		return d.validateBounds()
	case OpShow, OpHide, OpClose, OpDevTools:
		return ValidatePageID(d.PageID)
	case OpCloseProfile:
		return ValidateProfileID(d.ProfileID)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownOp, d.Op)
	}
}

func (d Directive) validateBounds() error {
	for name, value := range map[string]float64{"x": d.X, "y": d.Y, "w": d.W, "h": d.H} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("bounds %s is not a finite number", name)
		}
		if value < -maxPaneDIP || value > maxPaneDIP {
			return fmt.Errorf("bounds %s %g is outside +/-%g DIPs", name, value, maxPaneDIP)
		}
	}
	if d.W <= 0 || d.H <= 0 {
		return fmt.Errorf("bounds must have positive width and height, got %gx%g", d.W, d.H)
	}
	return nil
}

// ReportKind is the closed vocabulary of the launcher's answer.
type ReportKind string

const (
	// ReportCreated carries the page's CDP targetId as its detail, which
	// is what lets the backend attach to a controller it did not create.
	ReportCreated ReportKind = "created"
	// ReportCreateFailed carries the failure text.
	ReportCreateFailed ReportKind = "create-failed"
	// ReportClosed acknowledges a close (directive-driven or not).
	ReportClosed ReportKind = "closed"
	// ReportProcessFailed reports a browser/renderer process death for a
	// page, so the backend can retire its page handle instead of waiting
	// on a target that is gone.
	ReportProcessFailed ReportKind = "process-failed"
)

// RPCReport is the method name the launcher posts its answers under, over
// the same notification-bridge connection the directive arrived on. It is
// spelled here rather than in the launcher so the backend's receiver and
// the launcher's caller cannot drift.
const RPCReport = "BrowserHostReport"

// MaxReportDetailBytes bounds the detail string. A targetId is ~32 hex
// characters and an error is a sentence; anything larger is a bug
// upstream, and the report rides the notification bridge whose read limit
// protects the backend, not the launcher.
const MaxReportDetailBytes = 4096

// ValidKind reports whether kind is one the backend will recognise. The
// launcher checks its OWN reports with it, so a typo in launcher code
// fails at the call site instead of at the far end.
func ValidKind(kind ReportKind) bool {
	switch kind {
	case ReportCreated, ReportCreateFailed, ReportClosed, ReportProcessFailed:
		return true
	default:
		return false
	}
}

// CDPTunnelPath is the backend route the launcher dials to carry CDP
// traffic. Windows -> WSL, the same direction and credential as the
// notification bridge: nothing ever listens across the WSL boundary.
const CDPTunnelPath = "/browser-cdp"

// TruncateDetail bounds a detail string on a rune boundary.
func TruncateDetail(detail string) string {
	if len(detail) <= MaxReportDetailBytes {
		return detail
	}
	cut := MaxReportDetailBytes
	for cut > 0 && !isUTF8Start(detail[cut]) {
		cut--
	}
	return strings.TrimSpace(detail[:cut])
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }
