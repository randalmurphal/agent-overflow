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
//
// Two fixtures, because a page's cost now depends on its SHAPE
// (docs/architecture/timeline-window-pages.md §2): the rows it ships are
// its prose plus `RunWindowRows` members of each activity run, and a run
// of any length costs one stub beyond that. heavyThreadShape is the
// run-dominated extreme — one 91-member run holding most of the thread's
// weight — and proseRunThreadShape is the ordinary conversation, where
// prose rows the page must ship whole carry the bytes.

// wireRunWindowRows is the client setting both fixtures state their
// ceilings for: the `activityRunWindowRows` default. The numbers below
// describe pages of this shape and no other.
const wireRunWindowRows = 30

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

// proseRunThreadShape is the ordinary conversation: 18 turns of one prose
// answer followed by a 10-member tool run, 198 rows in all. No run is
// longer than the run window, so nothing here is shortened by shipping
// fewer members — every row the page covers rides the wire, which is the
// shape the run window cannot help with and the one most threads have.
// The prose rows carry 1,400 B summaries, a real assistant answer's
// preview, against the 240 B a tool row's summary averages.
func proseRunThreadShape() []heavyThreadRow {
	rows := make([]heavyThreadRow, 0, 198)
	for range 18 {
		rows = append(rows, heavyThreadRow{kind: "assistant_text", summaryBytes: 1400})
		for j := range 5 {
			size := 90
			if j == 0 {
				size = 4200
			}
			rows = append(rows, heavyThreadRow{kind: "tool_call", inputBytes: size})
			rows = append(rows, heavyThreadRow{
				kind: "tool_completion", previewBytes: 600, patchBytes: 400, spanBytes: 200,
			})
		}
	}
	return rows
}

// What each fixture's cold window ships, at wireRunWindowRows.
//
// heavyThreadShape's 200 rows group into ONE 91-member run (its rows
// alternate tool_call and tool_completion until the completions run out
// at row 90), then 55 prose rows alternating with 54 single-member runs.
// The page therefore ships 30 members of the long run, all 54 singletons
// and all 55 prose rows, and carries a stub for each of the 55 runs. The
// 61 members the long run did not ship cost one stub between them, which
// is the whole point: the run could have been 5,000 rows for the same
// bytes.
//
// proseRunThreadShape's 198 rows group into 18 runs of 10, none longer
// than the run window, so every row ships and each run still carries the
// stub that accounts for it.
const (
	heavyPageRows  = 139
	heavyPageStubs = 55
	prosePageRows  = 198
	prosePageStubs = 18
)

// Ceilings for the cold window on each fixture. Both directions are
// budgets, for different clients. Deflated bytes are what a remote peer
// pays and what §14 states its ~50 KB budget in; raw bytes are what the
// embedded webview and any loopback peer pay, since permessage-deflate is
// only negotiated off-loopback.
//
// Headroom is sized to absorb incidental growth — a field added to
// store.Item, a longer status string — without absorbing a lost elision
// or a run window that stopped bounding, either of which is worth tens of
// KB on these windows.
const (
	// Run-dominated, default client (collapseDiffPreviews on): measured
	// 124,737 raw / 22,300 deflated for 139 rows and 55 stubs. The
	// deflated ceiling is set below §14's budget on purpose: passing this
	// test is the statement that a cold attach fits, not that it nearly
	// fits.
	coldWindowRawCeiling        = 136 << 10 // 12% over measured
	coldWindowCompressedCeiling = 25 << 10  // 15% over measured, under the §14 budget

	// The same window for a client that asked for inline previews and
	// pays for its patch text: measured 143,172 raw / 25,475 deflated. It
	// stays under the unprojected window because meta.input elision
	// applies to every client — the preference governs previews, not the
	// whole projection.
	coldWindowPreviewsOnRawCeiling        = 156 << 10 // 12% over measured
	coldWindowPreviewsOnCompressedCeiling = 28 << 10  // 13% over measured

	// The conversation shape, where all 198 rows ship: measured 227,955
	// raw / 41,080 deflated with previews off, 290,415 / 50,850 with them
	// on. Nearly twice the run-dominated page's bytes for 1.4x the rows,
	// which is the point of pinning both — a budget stated only against a
	// page that ships 30 of a run's 91 members says nothing about the
	// page a normal thread loads.
	proseWindowRawCeiling                  = 250 << 10 // 12% over measured
	proseWindowCompressedCeiling           = 46 << 10  // 15% over measured
	proseWindowPreviewsOnRawCeiling        = 310 << 10 // 9% over measured
	proseWindowPreviewsOnCompressedCeiling = 56 << 10  // 13% over measured
)

func TestColdWindowWireBudget(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	page, err := app.ListThreadSliceAround(thread.ID, "", 200, PageShape{RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	assertPageShape(t, "run-dominated", page, heavyPageRows, heavyPageStubs)
	raw, compressed := coldWindowWire(t, page)
	t.Logf("cold window, previews off: %d raw, %d deflated", raw, compressed)
	assertWireCeilings(t, "cold window", raw, compressed,
		coldWindowRawCeiling, coldWindowCompressedCeiling)

	on, err := app.ListThreadSliceAround(thread.ID, "", 200,
		PageShape{InlinePreviews: true, RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews on): %v", err)
	}
	assertPageShape(t, "run-dominated, previews on", on, heavyPageRows, heavyPageStubs)
	rawOn, compressedOn := coldWindowWire(t, on)
	t.Logf("cold window, previews on: %d raw, %d deflated", rawOn, compressedOn)
	assertWireCeilings(t, "previews-on window", rawOn, compressedOn,
		coldWindowPreviewsOnRawCeiling, coldWindowPreviewsOnCompressedCeiling)
}

// The run-dominated fixture proves a long run costs a stub; this proves
// the other half of the shape rule, that a page whose runs all fit the
// window ships every row it covers and still pays a stub for each run.
func TestProseWindowWireBudget(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, proseRunThreadShape())

	page, err := app.ListThreadSliceAround(thread.ID, "", 200, PageShape{RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	assertPageShape(t, "prose-dominated", page, prosePageRows, prosePageStubs)
	for _, stub := range page.Runs {
		if stub.UnshippedBefore != 0 || stub.UnshippedAfter != 0 {
			t.Fatalf("run %s left %d/%d members unshipped; no run here is longer than the window",
				stub.FirstItemID, stub.UnshippedBefore, stub.UnshippedAfter)
		}
	}
	raw, compressed := coldWindowWire(t, page)
	t.Logf("prose window, previews off: %d raw, %d deflated", raw, compressed)
	assertWireCeilings(t, "prose window", raw, compressed,
		proseWindowRawCeiling, proseWindowCompressedCeiling)

	on, err := app.ListThreadSliceAround(thread.ID, "", 200,
		PageShape{InlinePreviews: true, RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews on): %v", err)
	}
	assertPageShape(t, "prose-dominated, previews on", on, prosePageRows, prosePageStubs)
	rawOn, compressedOn := coldWindowWire(t, on)
	t.Logf("prose window, previews on: %d raw, %d deflated", rawOn, compressedOn)
	assertWireCeilings(t, "prose previews-on window", rawOn, compressedOn,
		proseWindowPreviewsOnRawCeiling, proseWindowPreviewsOnCompressedCeiling)
}

// TestColdWindowWireBudget_ProjectionIsWhatMakesIt keeps the gate
// honest. Ceilings a fixture would meet on its own pass forever while
// the mechanism rots underneath them, so this measures the same window
// unprojected and fails if the difference has gone.
//
// It measures the PROSE fixture: every row of that page ships, so the
// projection is the only thing between the store's rows and the wire.
// On the run-dominated fixture the run window has already dropped most
// of the rows carrying elidable fields, which is the other mechanism and
// is measured on its own below.
func TestColdWindowWireBudget_ProjectionIsWhatMakesIt(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, proseRunThreadShape())

	projected, err := app.ListThreadSliceAround(thread.ID, "", 200, PageShape{RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	// The same page from the store, so the difference measured is the
	// projection alone and not the run window the shape already applied.
	unprojected, err := app.store.ListThreadSliceAround(thread.ID, "", 200, wireRunWindowRows)
	if err != nil {
		t.Fatalf("store.ListThreadSliceAround: %v", err)
	}
	if len(unprojected.Items) != len(projected.Items) {
		t.Fatalf("store page has %d rows, projected page %d; the difference must be bytes, not rows",
			len(unprojected.Items), len(projected.Items))
	}

	beforeRaw, beforeCompressed := coldWindowWire(t, unprojected)
	afterRaw, afterCompressed := coldWindowWire(t, projected)
	t.Logf("unprojected: %d raw, %d deflated", beforeRaw, beforeCompressed)
	t.Logf("projected:   %d raw, %d deflated (%.0f%% / %.0f%% of it)",
		afterRaw, afterCompressed,
		100*float64(afterRaw)/float64(beforeRaw),
		100*float64(afterCompressed)/float64(beforeCompressed))

	// Asserting a fifth leaves the fixture room to drift without letting
	// a projection that stopped eliding through.
	if beforeRaw-afterRaw < beforeRaw/5 {
		t.Errorf("projection saved %d of %d raw bytes; expected at least a fifth",
			beforeRaw-afterRaw, beforeRaw)
	}
	if beforeCompressed-afterCompressed < beforeCompressed/5 {
		t.Errorf("projection saved %d of %d deflated bytes; expected at least a fifth",
			beforeCompressed-afterCompressed, beforeCompressed)
	}
}

// The run window is the other mechanism holding these ceilings, and the
// one the new page shape adds: on a thread whose weight is one long run,
// the page's cost is set by how many of its members ship, not by how
// many rows the range covers. Measured against the same page with the
// window opened to its ceiling, both projected, so the run window is the
// only difference.
func TestColdWindowWireBudget_TheRunWindowIsWhatMakesIt(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	windowed, err := app.ListThreadSliceAround(thread.ID, "", 200, PageShape{RunWindowRows: wireRunWindowRows})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	wide, err := app.ListThreadSliceAround(thread.ID, "", 200, PageShape{RunWindowRows: 200})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(wide run window): %v", err)
	}
	if len(wide.Items) != 200 {
		t.Fatalf("wide window ships %d rows, want the whole 200-row range", len(wide.Items))
	}

	wideRaw, wideCompressed := coldWindowWire(t, wide)
	rawBytes, compressed := coldWindowWire(t, windowed)
	t.Logf("run window %d: %d raw, %d deflated; run window 200: %d raw, %d deflated",
		wireRunWindowRows, rawBytes, compressed, wideRaw, wideCompressed)

	// The 61 members the window leaves behind are the fixture's heaviest
	// rows. Asserting a third leaves room for the fixture to drift
	// without letting a page that ships whole runs through.
	if wideRaw-rawBytes < wideRaw/3 {
		t.Errorf("the run window saved %d of %d raw bytes; expected at least a third",
			wideRaw-rawBytes, wideRaw)
	}
	if wideCompressed-compressed < wideCompressed/3 {
		t.Errorf("the run window saved %d of %d deflated bytes; expected at least a third",
			wideCompressed-compressed, wideCompressed)
	}
	// And the rows it left behind are still accounted for: the page's
	// range did not shrink with them.
	if got := totalRunMembers(windowed); got != totalRunMembers(wide) {
		t.Errorf("the windowed page accounts for %d run members, the wide one %d",
			got, totalRunMembers(wide))
	}
}

// totalRunMembers is every physical member the page's stubs describe.
func totalRunMembers(page store.PagedItems) int {
	total := 0
	for _, stub := range page.Runs {
		total += stub.MemberCount
	}
	return total
}

// assertPageShape pins what the ceilings are stated for: the rows and
// stubs a page of this shape ships. A ceiling met by shipping fewer rows
// than the shape calls for is not the budget this file records.
func assertPageShape(t *testing.T, what string, page store.PagedItems, rows, stubs int) {
	t.Helper()
	if len(page.Items) != rows {
		t.Fatalf("%s: page ships %d rows, want the %d these ceilings are stated for",
			what, len(page.Items), rows)
	}
	if len(page.Runs) != stubs {
		t.Fatalf("%s: page carries %d run stubs, want %d", what, len(page.Runs), stubs)
	}
}

func assertWireCeilings(t *testing.T, what string, raw, compressed, rawCeiling, compressedCeiling int) {
	t.Helper()
	if raw > rawCeiling {
		t.Errorf("%s is %d raw bytes, over the %d ceiling", what, raw, rawCeiling)
	}
	if compressed > compressedCeiling {
		t.Errorf("%s is %d deflated bytes, over the %d ceiling", what, compressed, compressedCeiling)
	}
}
