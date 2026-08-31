package app

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The wire budget gate docs/specs/remote-access.md §14 calls for: seed a
// deterministic heavy thread, serialize the cold window exactly as the
// socket does, and fail on ceilings for the bytes that leave the
// process. A budget kept in a commit message decays; a budget kept in a
// test fails on the commit that breaks it.
//
// Why a Go test and not a harness scenario (docs/architecture/
// agent-harness.md): the harness boots the real SPA under a browser
// driven by Playwright, which buys UI fidelity this measurement does not
// need and costs determinism it does. Counting here puts the gate in
// `make go-test` on every commit rather than in `make e2e`, and lets it
// deflate the frame with the socket's own settings instead of reading a
// number back out through the browser.
//
// What "exactly as the wire does" means here: the RPC result is
// marshalled into transport.ServerFrame — the struct the connection
// writes — and deflated at flate.BestSpeed, the level coder/websocket
// uses for permessage-deflate (compress.go). Production negotiates that
// mode for non-loopback peers, which is the client this budget exists
// for. Context takeover carries the window across messages, and a cold
// attach is the first message on a fresh connection, so a fresh flate
// stream is byte-for-byte what that client receives.

// coldWindowWire returns the raw and deflated size of one RPC response
// frame carrying the given page.
func coldWindowWire(t *testing.T, page store.PagedItems) (raw int, compressed int) {
	t.Helper()
	result, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	frame, err := json.Marshal(transport.ServerFrame{
		Type:   "rpc",
		ID:     "1",
		Result: result,
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := writer.Write(frame); err != nil {
		t.Fatalf("deflate frame: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close deflate: %v", err)
	}
	return len(frame), buf.Len()
}

// Ceilings for the 200-row cold window on the deterministic heavy thread
// (heavyThreadShape in app_item_projection_test.go, built to the field
// split §14 measured on a real 65,877-item thread). The fixture arrives
// at 298,501 raw / 59,917 deflated bytes unprojected, against the
// 330 KB / 59 KB §14 recorded, so these numbers are answering the same
// question the measurement asked.
//
// Both directions are budgets, for different clients. Deflated bytes are
// what a remote peer pays and what §14 states its ~50 KB budget in; raw
// bytes are what the embedded webview and any loopback peer pay, since
// permessage-deflate is only negotiated off-loopback.
//
// Headroom is sized to absorb incidental growth — a field added to
// store.Item, a longer status string — without absorbing a lost
// elision, which on this window is worth tens of KB.
const (
	// Default client (collapseDiffPreviews on): measured 193,096 raw /
	// 38,423 deflated. The deflated ceiling is set below §14's budget on
	// purpose: passing this test is the statement that a cold attach
	// fits, not that it nearly fits.
	coldWindowRawCeiling        = 216 << 10 // 12% over measured
	coldWindowCompressedCeiling = 44 << 10  // 15% over measured, under the §14 budget

	// A client that asked for inline previews keeps its patch text and
	// pays for it: measured 248,401 raw / 49,045 deflated. It stays
	// under the unprojected window because meta.input elision applies to
	// every client — the preference governs previews, not the whole
	// projection.
	coldWindowPreviewsOnRawCeiling        = 272 << 10 // 9% over measured
	coldWindowPreviewsOnCompressedCeiling = 54 << 10  // 13% over measured
)

func TestColdWindowWireBudget(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	page, err := app.ListThreadSliceAround(thread.ID, "", 200, false)
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	// The byte backstop is sized so it never trims a window this shape;
	// if it starts to, the ceilings below stop describing 200 rows.
	if len(page.Items) != 200 {
		t.Fatalf("window is %d rows, want the 200 these ceilings are stated for", len(page.Items))
	}
	raw, compressed := coldWindowWire(t, page)
	t.Logf("cold window, previews off: %d raw, %d deflated", raw, compressed)
	if raw > coldWindowRawCeiling {
		t.Errorf("cold window is %d raw bytes, over the %d ceiling", raw, coldWindowRawCeiling)
	}
	if compressed > coldWindowCompressedCeiling {
		t.Errorf("cold window is %d deflated bytes, over the %d ceiling", compressed, coldWindowCompressedCeiling)
	}

	on, err := app.ListThreadSliceAround(thread.ID, "", 200, true)
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews on): %v", err)
	}
	rawOn, compressedOn := coldWindowWire(t, on)
	t.Logf("cold window, previews on: %d raw, %d deflated", rawOn, compressedOn)
	if rawOn > coldWindowPreviewsOnRawCeiling {
		t.Errorf("previews-on window is %d raw bytes, over the %d ceiling", rawOn, coldWindowPreviewsOnRawCeiling)
	}
	if compressedOn > coldWindowPreviewsOnCompressedCeiling {
		t.Errorf("previews-on window is %d deflated bytes, over the %d ceiling",
			compressedOn, coldWindowPreviewsOnCompressedCeiling)
	}
}

// TestColdWindowWireBudget_ProjectionIsWhatMakesIt keeps the gate
// honest. Ceilings a fixture would meet on its own pass forever while
// the mechanism rots underneath them, so this measures the same window
// unprojected and fails if the difference has gone.
func TestColdWindowWireBudget_ProjectionIsWhatMakesIt(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	projected, err := app.ListThreadSliceAround(thread.ID, "", 200, false)
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	unprojected, err := app.store.ListThreadSliceAround(thread.ID, "", 200)
	if err != nil {
		t.Fatalf("store.ListThreadSliceAround: %v", err)
	}

	beforeRaw, beforeCompressed := coldWindowWire(t, unprojected)
	afterRaw, afterCompressed := coldWindowWire(t, projected)
	t.Logf("unprojected: %d raw, %d deflated", beforeRaw, beforeCompressed)
	t.Logf("projected:   %d raw, %d deflated (%.0f%% / %.0f%% of it)",
		afterRaw, afterCompressed,
		100*float64(afterRaw)/float64(beforeRaw),
		100*float64(afterCompressed)/float64(beforeCompressed))

	// Measured: the projection removes 35% of the raw bytes and 36% of
	// the deflated ones. Asserting a fifth leaves the fixture room to
	// drift without letting a projection that stopped eliding through.
	if beforeRaw-afterRaw < beforeRaw/5 {
		t.Errorf("projection saved %d of %d raw bytes; expected at least a fifth",
			beforeRaw-afterRaw, beforeRaw)
	}
	if beforeCompressed-afterCompressed < beforeCompressed/5 {
		t.Errorf("projection saved %d of %d deflated bytes; expected at least a fifth",
			beforeCompressed-afterCompressed, beforeCompressed)
	}
}
