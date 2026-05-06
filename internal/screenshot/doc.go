// Package screenshot drives a headless Chromium subprocess via the
// Chrome DevTools Protocol to capture pixel-faithful renders of pages
// served by the local design file server.
//
// The previous in-iframe capture path (modern-screenshot's SVG
// foreignObject) couldn't load web fonts because the iframe is
// sandbox="allow-scripts" without allow-same-origin, so the SVG
// rendering context's fetch() of @font-face URLs was cross-origin and
// blocked. The captured canvas fell back to system fonts; everything
// downstream of font metrics (line wrapping, ch-relative widths,
// pseudo-element typography) diverged from what the user saw.
//
// This package replaces that path with a real browser engine. We
// download chrome-headless-shell from Chrome-for-Testing on first
// capture, cache it under the user config dir, run it as a long-lived
// subprocess, navigate it at the same loopback URL the user's webview
// loads, race document.fonts.ready against a 4 s soft cap, scroll
// the document to settle lazy content, and call
// Page.captureScreenshot{captureBeyondViewport} for a full-page PNG. Tile slicing happens in pure Go after the
// capture so the existing per-image vision-token budget contract
// stays the same.
//
// We render in Chromium even when the user's webview is WebKit
// (macOS/Linux). For typical UI work the engine difference is small
// compared to the divergence the previous path produced; for byte-
// for-byte WYSIWYG it isn't, but that wasn't an option without
// forking Wails and writing three CGO bridges.
package screenshot
