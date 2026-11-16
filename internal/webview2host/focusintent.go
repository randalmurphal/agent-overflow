package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Page focus intents on the launcher. The address bar's Enter and Escape ask
// that the page take the keyboard (OpFocus), and the user's next focus choice
// in the SPA withdraws the request. The directive crosses to the backend and
// back through WSL; the withdrawal must win even when it is sent while the
// directive is still on its way, so the SPA posts each arm and withdrawal
// straight to the launcher (chrome.webview.postMessage, like its pane
// geometry), and the launcher drops a directive whose intent it has heard
// withdrawn. The backend's half is internal/browser/focus_intent.go.

// FocusMessagePrefix starts every focus intent message the SPA posts.
// Mirrored by frontend/src/lib/native/paneGeometryBridge.ts.
const FocusMessagePrefix = "ao:pane-focus:"

// FocusCapabilityGlobal is the window property the launcher sets in the SPA
// document to say it takes focus intent messages; FocusCapabilityScript
// sets it.
const FocusCapabilityGlobal = "__aoPaneFocusBridge"

// FocusCapabilityScript is what the launcher evaluates in each document its
// window loads to advertise the capability.
const FocusCapabilityScript = "window." + FocusCapabilityGlobal + " = 1;"

// FocusIntentMaxAge bounds how long an intent stands. The backend keeps one
// waiting this long for its page to be presented, and the launcher applies
// none older, counted from when it heard the arm.
const FocusIntentMaxAge = 10 * time.Second

// maxFocusMessageBytes bounds one message; a message is a mount id and two
// small fields.
const maxFocusMessageBytes = 512

// FocusIntentMessage is one arm or withdrawal. Intent numbers increase per
// mount from 1.
type FocusIntentMessage struct {
	Mount     string `json:"mount"`
	Intent    uint64 `json:"intent"`
	Withdrawn bool   `json:"withdrawn"`
}

// ErrNotFocusIntent is ParseFocusIntentMessage's answer for a raw message
// that is not a focus intent message.
var ErrNotFocusIntent = errors.New("not a pane focus message")

// ParseFocusIntentMessage decodes and validates one raw message.
func ParseFocusIntentMessage(message string) (FocusIntentMessage, error) {
	body, ok := strings.CutPrefix(message, FocusMessagePrefix)
	if !ok {
		return FocusIntentMessage{}, ErrNotFocusIntent
	}
	if len(message) > maxFocusMessageBytes {
		return FocusIntentMessage{}, fmt.Errorf("pane focus message of %d bytes exceeds %d", len(message), maxFocusMessageBytes)
	}
	var msg FocusIntentMessage
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		return FocusIntentMessage{}, fmt.Errorf("decode pane focus message: %w", err)
	}
	if err := validateToken("mount id", msg.Mount, maxPageIDLen); err != nil {
		return FocusIntentMessage{}, err
	}
	if msg.Intent == 0 {
		return FocusIntentMessage{}, errors.New("pane focus intent must start at 1")
	}
	return msg, nil
}

// FocusReport is the detail of a ReportFocused or ReportFocusFailed: the
// intent the move was made for, and a refusal's error.
type FocusReport struct {
	Mount  string `json:"mount"`
	Intent uint64 `json:"intent"`
	Error  string `json:"error,omitempty"`
}

// Encode answers the report as a report detail.
func (r FocusReport) Encode() string {
	data, err := json.Marshal(r)
	if err != nil {
		// Strings and an integer always encode.
		panic(err)
	}
	return string(data)
}

// ParseFocusReport decodes a focus report's detail.
func ParseFocusReport(detail string) (FocusReport, error) {
	var r FocusReport
	if err := json.Unmarshal([]byte(detail), &r); err != nil {
		return FocusReport{}, fmt.Errorf("decode focus report: %w", err)
	}
	if err := validateToken("mount id", r.Mount, maxPageIDLen); err != nil {
		return FocusReport{}, err
	}
	if r.Intent == 0 {
		return FocusReport{}, errors.New("focus report names no intent")
	}
	return r, nil
}

// FocusIntents is the launcher's record of each mount's newest focus intent:
// its number, whether it was withdrawn, when the arm was heard, and the page
// a directive moved focus into for it. Messages arrive on goroutines of
// their own; directives are decided on the UI thread. Neither waits on
// anything but this lock.
type FocusIntents struct {
	mu      sync.Mutex
	entries map[string]*focusIntentEntry
	tick    uint64
	now     func() time.Time
}

type focusIntentEntry struct {
	intent    uint64
	withdrawn bool
	armed     time.Time
	// applied is the page a directive moved focus into for intent.
	applied string
	touched uint64
}

// NewFocusIntents returns an empty record.
func NewFocusIntents() *FocusIntents {
	return &FocusIntents{entries: make(map[string]*focusIntentEntry), now: time.Now}
}

// Note records an arm or a withdrawal. A message for an intent older than the
// mount's newest is stale and changes nothing. It answers the page to take
// the keyboard back from: one a directive already moved focus into for the
// intent this withdraws. The caller hands it back on the UI thread if the
// page still holds it (Host.ReleasePageFocus).
func (f *FocusIntents) Note(msg FocusIntentMessage) (handBack string) {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entries[msg.Mount]
	switch {
	case entry == nil:
		entry = f.addLocked(msg.Mount)
	case msg.Intent < entry.intent:
		return ""
	case msg.Intent == entry.intent:
		if msg.Withdrawn && !entry.withdrawn {
			entry.withdrawn = true
			handBack, entry.applied = entry.applied, ""
		}
		return handBack
	}
	f.tick++
	*entry = focusIntentEntry{intent: msg.Intent, withdrawn: msg.Withdrawn, armed: f.now(), touched: f.tick}
	return ""
}

// Claim answers whether a focus directive for the mount's intent may move
// focus into pageID now, and if so records the move: the SPA has not
// withdrawn it or armed a newer one, and it is not older than
// FocusIntentMaxAge. A directive that arrives before its arm is claimed, and
// the arm that follows changes nothing.
func (f *FocusIntents) Claim(mount string, intent uint64, pageID string) bool {
	if f == nil {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entries[mount]
	if entry == nil {
		entry = f.addLocked(mount)
	}
	switch {
	case intent < entry.intent:
		return false
	case intent == entry.intent && entry.withdrawn:
		return false
	case intent == entry.intent && f.now().Sub(entry.armed) > FocusIntentMaxAge:
		return false
	case intent > entry.intent:
		f.tick++
		*entry = focusIntentEntry{intent: intent, armed: f.now(), touched: f.tick}
	}
	entry.applied = pageID
	return true
}

// addLocked adds a mount's entry, evicting the least recently updated mount
// at MaxGeometryMounts (the backend's bound on mounted panes).
func (f *FocusIntents) addLocked(mount string) *focusIntentEntry {
	if len(f.entries) >= MaxGeometryMounts {
		oldest := ""
		var at uint64
		for id, entry := range f.entries {
			if oldest == "" || entry.touched < at {
				oldest, at = id, entry.touched
			}
		}
		delete(f.entries, oldest)
	}
	f.tick++
	entry := &focusIntentEntry{touched: f.tick}
	f.entries[mount] = entry
	return entry
}
