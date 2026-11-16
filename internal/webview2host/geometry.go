package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// Local pane geometry: the SPA window and the pane controllers live in the
// launcher process on one UI thread, so the launcher hears where a pane
// moved straight from the SPA (chrome.webview.postMessage, which reaches
// Wails' raw message handler) instead of waiting for the report to cross to
// the backend and its directive to come back. The backend still receives
// every report: it owns the page's viewport and which mount presents. This
// file is the message, its validation, and the mailbox; the placement it
// feeds is presenter.go.

// GeometryMessagePrefix starts every geometry message the SPA posts. Wails
// handles messages starting "wails:" itself and hands the rest to the raw
// message handler. Mirrored by frontend/src/lib/native/paneGeometryBridge.ts.
const GeometryMessagePrefix = "ao:pane-geometry:"

// GeometryCapabilityGlobal is the window property the launcher sets in the
// SPA document to say it takes geometry messages. A SPA posts only where it
// is set, so an older launcher is never sent messages it cannot read.
const GeometryCapabilityGlobal = "__aoPaneGeometryBridge"

// GeometryCapabilityScript is what the launcher evaluates in each document
// its window loads to advertise the capability.
const GeometryCapabilityScript = "window." + GeometryCapabilityGlobal + " = 1;"

// maxGeometryMessageBytes bounds one message. A report is a few hundred
// bytes; anything larger is not one.
const maxGeometryMessageBytes = 4096

// MaxGeometryMounts bounds the mailbox. It matches the backend's bound on
// mounted panes (maxCompanionPanes), so a live mount is never evicted by
// the mounts of one process.
const MaxGeometryMounts = 64

// PaneGeometry is one report of where a mounted browser pane's host rect
// sits, in the SPA's CSS pixels, as the SPA measured it. Mount is the
// backend's id for the pane mount and Seq the SPA's per-mount count, which
// orders reports that Wails delivers on goroutines of their own.
type PaneGeometry struct {
	Mount            string  `json:"mount"`
	Seq              uint64  `json:"seq"`
	X                float64 `json:"x"`
	Y                float64 `json:"y"`
	Width            float64 `json:"width"`
	Height           float64 `json:"height"`
	ClipX            float64 `json:"clipX"`
	ClipY            float64 `json:"clipY"`
	ClipWidth        float64 `json:"clipWidth"`
	ClipHeight       float64 `json:"clipHeight"`
	ViewportWidth    float64 `json:"viewportWidth"`
	ViewportHeight   float64 `json:"viewportHeight"`
	DevicePixelRatio float64 `json:"devicePixelRatio"`
	Visible          bool    `json:"visible"`
	Background       string  `json:"background"`
}

// ErrNotGeometry is ParseGeometryMessage's answer for a raw message that is
// not a geometry message at all, which a handler ignores rather than logs.
var ErrNotGeometry = errors.New("not a pane geometry message")

// ParseGeometryMessage decodes and validates one raw message.
func ParseGeometryMessage(message string) (PaneGeometry, error) {
	body, ok := strings.CutPrefix(message, GeometryMessagePrefix)
	if !ok {
		return PaneGeometry{}, ErrNotGeometry
	}
	if len(message) > maxGeometryMessageBytes {
		return PaneGeometry{}, fmt.Errorf("pane geometry message of %d bytes exceeds %d", len(message), maxGeometryMessageBytes)
	}
	// Unknown fields are ignored: a newer SPA may say more than this
	// launcher reads.
	var g PaneGeometry
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		return PaneGeometry{}, fmt.Errorf("decode pane geometry: %w", err)
	}
	if err := g.Validate(); err != nil {
		return PaneGeometry{}, err
	}
	return g, nil
}

// Validate holds a report to the raw geometry contract. It moves a real
// window, so its numbers meet a bounds directive's bounds
// (paneBounds.validateNumbers), but its clip is the measured visible
// intersection, computed per axis: a pane scrolled out on one axis reports
// an empty clip on that axis alone. An empty clip on either axis is a hidden
// pane (presenter.go), not a malformed report.
func (g PaneGeometry) Validate() error {
	if err := validateToken("mount id", g.Mount, maxPageIDLen); err != nil {
		return err
	}
	if g.Seq == 0 {
		return errors.New("pane geometry sequence must start at 1")
	}
	return g.bounds().validateNumbers()
}

// bounds is the report's whole host rect as a paneBounds.
func (g PaneGeometry) bounds() paneBounds {
	return paneBounds{
		X: g.X, Y: g.Y, W: g.Width, H: g.Height,
		CX: g.ClipX, CY: g.ClipY, CW: g.ClipWidth, CH: g.ClipHeight,
		VW: g.ViewportWidth, VH: g.ViewportHeight, DPR: g.DevicePixelRatio,
		Bg: g.Background,
	}
}

// TrustedGeometrySource reports whether a raw message came from the SPA the
// launcher loaded: both the sending document and the top-level document are
// on the backend page's origin (scheme and host:port). The picker, an error
// page and any other origin are refused.
func TrustedGeometrySource(source, topSource, pageURL string) bool {
	want, ok := originOf(pageURL)
	if !ok {
		return false
	}
	got, ok := originOf(source)
	top, topOK := originOf(topSource)
	return ok && topOK && got == want && top == want
}

func originOf(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), true
}

// GeometryMailbox keeps the newest report per mount. Reports arrive on
// goroutines of their own and in no fixed order; one older than the held
// report is dropped. The UI thread drains it with one callback at a time:
// Put answers true only when no drain is pending, and the caller then
// schedules exactly one (never one per message), which calls Take. Neither
// method waits on anything but the mailbox's own short lock.
type GeometryMailbox struct {
	mu      sync.Mutex
	entries map[string]*geometryEntry
	pending bool
	// tick orders entries by last update, for eviction.
	tick uint64
}

type geometryEntry struct {
	geometry PaneGeometry
	dirty    bool
	touched  uint64
}

// NewGeometryMailbox returns an empty mailbox.
func NewGeometryMailbox() *GeometryMailbox {
	return &GeometryMailbox{entries: make(map[string]*geometryEntry)}
}

// Put records a validated report. It answers whether the caller must
// schedule a drain. At MaxGeometryMounts the least recently updated mount is
// evicted; a page bound to it falls back to the backend's bounds.
func (m *GeometryMailbox) Put(g PaneGeometry) (schedule bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[g.Mount]
	if entry != nil && g.Seq <= entry.geometry.Seq {
		return false
	}
	if entry == nil {
		if len(m.entries) >= MaxGeometryMounts {
			m.evictOldestLocked()
		}
		entry = &geometryEntry{}
		m.entries[g.Mount] = entry
	}
	m.tick++
	entry.geometry, entry.dirty, entry.touched = g, true, m.tick
	if m.pending {
		return false
	}
	m.pending = true
	return true
}

func (m *GeometryMailbox) evictOldestLocked() {
	oldest := ""
	var at uint64
	for mount, entry := range m.entries {
		if oldest == "" || entry.touched < at {
			oldest, at = mount, entry.touched
		}
	}
	delete(m.entries, oldest)
}

// Take returns every report put since the last Take and ends the pending
// drain, so a report put after this schedules the next one.
func (m *GeometryMailbox) Take() []PaneGeometry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = false
	var out []PaneGeometry
	for _, entry := range m.entries {
		if entry.dirty {
			entry.dirty = false
			out = append(out, entry.geometry)
		}
	}
	return out
}

// Latest answers the newest report for a mount.
func (m *GeometryMailbox) Latest(mount string) (PaneGeometry, bool) {
	if m == nil || mount == "" {
		return PaneGeometry{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[mount]
	if entry == nil {
		return PaneGeometry{}, false
	}
	return entry.geometry, true
}
