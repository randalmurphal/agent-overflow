// Package ids holds the identifiers internal/browser shares with its
// subpackages, so each kind is one type both sides take and a value of one
// kind cannot be passed where another is meant. A kind only the browser
// package uses is declared there. It imports nothing.
package ids

// PageHandle is an engine's page handle, unique for the engine's life: a
// headless Chromium target id, a launcher page id on the hosted engine, or
// a WebKit engine's page number. It names an engine page in the engine's
// reports and calls, and a popup in its admission (popupadmit); the
// Manager's own name for a tab is browser.PageID.
type PageHandle string
