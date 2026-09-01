package webview2host

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"agent-overflow/internal/keybindings"
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
	// OpClearData destroys the pane environment's site data wholesale: every
	// controller closes, the environment is released, and the user-data
	// folder is deleted and recreated empty. It addresses no page and no
	// profile — one folder holds every workspace's cookie jar, so "clear
	// site data" is that folder or nothing. PageID is a correlation id: the
	// backend waits on it exactly the way it waits on a create, and the
	// launcher answers ReportCleared or ReportClearFailed under it.
	OpClearData Op = "clear-data"
	// OpAccelerators replaces the host-wide set of chords a page must hand
	// back to AO instead of delivering to its document. The launcher matches
	// AcceleratorKeyPressed against it in its own process — the backend is a
	// network hop away and WebView2 wants the Handled answer synchronously —
	// and reports a match as ReportAccelerator. It addresses no page.
	OpAccelerators Op = "accelerators"
)

// Directive is one pane-host command. It is the JSON payload of an
// eventchan.BrowserHost frame.
//
// The zero value is not a valid directive: Validate rejects it, and the
// launcher validates BEFORE dispatching, so no field below is ever
// consumed unchecked.
const maxAccelerators = 2048

type Directive struct {
	Op        Op      `json:"op"`
	PageID    string  `json:"pageId,omitempty"`
	ProfileID string  `json:"profileId,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	W         float64 `json:"w,omitempty"`
	H         float64 `json:"h,omitempty"`
	// CX/CY/CW/CH are the VISIBLE clip rect in the same units as X/Y/W/H:
	// the controller keeps the full rect's size and the host crops its
	// presentation to this intersection, so a pane half behind the sidebar
	// shows its visible half instead of hiding or overhanging. A zero pair
	// means unclipped (crop to the full rect).
	CX float64 `json:"cx,omitempty"`
	CY float64 `json:"cy,omitempty"`
	CW float64 `json:"cw,omitempty"`
	CH float64 `json:"ch,omitempty"`
	// VW/VH are the SPA viewport the rect was measured in, in its CSS
	// pixels. The host scales the rect by its client size over these, so
	// the position is exact under webview zoom and any DPI. Zero means
	// unscaled (treat X/Y/W/H as client pixels).
	VW float64 `json:"vw,omitempty"`
	VH float64 `json:"vh,omitempty"`
	// Bg is the pane surface's resolved CSS color ("#rrggbb"), painted by
	// the controller where the page has not presented yet so freshly
	// exposed strips match the pane. Empty leaves the engine default.
	Bg string `json:"bg,omitempty"`
	// Accelerators is OpAccelerators' payload: the whole bound set, `mod`
	// already resolved by the backend.
	Accelerators []keybindings.Accelerator `json:"accelerators,omitempty"`
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
	case OpClearData:
		// Same strictness as a page op even though nothing here is a page:
		// the id is echoed back in a report and reaches log lines, and the
		// backend's waiter is keyed on it, so an unaddressable clear would
		// be a 45-second wait for an answer that can never arrive.
		return ValidatePageID(d.PageID)
	case OpCloseProfile:
		return ValidateProfileID(d.ProfileID)
	case OpAccelerators:
		// Defaults plus a full user override list is a few hundred; this is
		// a frame-size tripwire, not a policy.
		if len(d.Accelerators) > maxAccelerators {
			return fmt.Errorf("%d accelerators exceeds the %d cap", len(d.Accelerators), maxAccelerators)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownOp, d.Op)
	}
}

func (d Directive) validateBounds() error {
	for name, value := range map[string]float64{
		"x": d.X, "y": d.Y, "w": d.W, "h": d.H,
		"cx": d.CX, "cy": d.CY, "cw": d.CW, "ch": d.CH,
		"vw": d.VW, "vh": d.VH,
	} {
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
	// The clip is all-or-nothing like the viewport: a fully clipped pane is
	// hidden upstream, never sized to nothing here.
	if (d.CW > 0) != (d.CH > 0) || d.CW < 0 || d.CH < 0 {
		return fmt.Errorf("bounds clip must be a positive pair or absent, got %gx%g", d.CW, d.CH)
	}
	// The viewport pair is all-or-nothing: scaling one axis and not the
	// other could only misplace the view.
	if (d.VW > 0) != (d.VH > 0) || d.VW < 0 || d.VH < 0 {
		return fmt.Errorf("bounds viewport must be a positive pair or absent, got %gx%g", d.VW, d.VH)
	}
	if err := validateBg(d.Bg); err != nil {
		return err
	}
	return nil
}

// validateBg accepts "" or "#rrggbb". The value reaches a COM color struct
// and a log line, so anything fancier (rgb(), var(), names) is refused here
// rather than half-parsed at the host.
func validateBg(bg string) error {
	if bg == "" {
		return nil
	}
	if len(bg) != 7 || bg[0] != '#' {
		return fmt.Errorf("bounds bg %q is not #rrggbb", bg)
	}
	for i := 1; i < 7; i++ {
		c := bg[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return fmt.Errorf("bounds bg %q is not #rrggbb", bg)
		}
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
	// ReportCleared acknowledges a clear-data: the environment is released
	// and the user-data folder is empty again. Addressed by the clear's own
	// correlation id, never by a page.
	ReportCleared ReportKind = "cleared"
	// ReportClearFailed carries the last OS error the delete saw. Site data
	// the user asked to destroy is still on disk, which the backend must be
	// able to say out loud rather than report a silent success.
	ReportClearFailed ReportKind = "clear-failed"
	// ReportAccelerator carries one keybindings.Accelerator as JSON: a bound
	// chord the launcher took from the page named by the report's page id.
	ReportAccelerator ReportKind = "accelerator"
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
	case ReportCreated, ReportCreateFailed, ReportClosed, ReportProcessFailed,
		ReportCleared, ReportClearFailed, ReportAccelerator:
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
