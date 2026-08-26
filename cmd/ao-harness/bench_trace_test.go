package main

import (
	"strings"
	"testing"
)

// traceFixtureEvents is the minimal trace this parse has to get right:
// two forced events from one call site, one forced event from an
// anonymous frame, two layout/style events the ENGINE scheduled (no
// stack), one event whose `args` is not even an object, and one unrelated
// timeline event.
const traceFixtureEvents = `[
  {"name":"UpdateLayoutTree","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":1500,
   "args":{"beginData":{"stackTrace":[
     {"functionName":"measureRow","url":"http://x/Timeline.svelte","lineNumber":42},
     {"functionName":"caller","url":"http://x/a.js","lineNumber":1}]}}},
  {"name":"Layout","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":2500,
   "args":{"beginData":{"stackTrace":[
     {"functionName":"measureRow","url":"http://x/Timeline.svelte","lineNumber":42}]}}},
  {"name":"Layout","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":500,
   "args":{"beginData":{"stackTrace":[{"functionName":"","url":"http://x/b.js","lineNumber":7}]}}},
  {"name":"Layout","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":9000,
   "args":{"beginData":{"frame":"F"}}},
  {"name":"UpdateLayoutTree","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":9000},
  {"name":"Layout","cat":"disabled-by-default-devtools.timeline","ph":"X","dur":9000,"args":"not-an-object"},
  {"name":"Paint","cat":"devtools.timeline","ph":"X","dur":4000,"args":{}}
]`

func summarizeFixture(t *testing.T, document string) traceSummary {
	t.Helper()
	events, err := parseTraceEvents([]byte(document))
	if err != nil {
		t.Fatalf("parse trace: %v", err)
	}
	return summarizeForcedLayout(events)
}

func TestSummarizeForcedLayoutGroupsByTopFrame(t *testing.T) {
	summary := summarizeFixture(t, traceFixtureEvents)

	if summary.Events != 7 {
		t.Errorf("events = %d, want 7", summary.Events)
	}
	if summary.ForcedEvents != 3 {
		t.Errorf("forced = %d, want 3 (only the stacked ones)", summary.ForcedEvents)
	}
	// The three engine-scheduled ones: no args, args without beginData
	// stack, and args that are not an object at all. A stack is the signal
	// for "a script forced this"; its absence is a finding, not a parse
	// failure — and one malformed `args` must not fail the document.
	if summary.ScheduledEvents != 3 {
		t.Errorf("scheduled = %d, want 3", summary.ScheduledEvents)
	}
	if summary.ForcedMs != 4.5 {
		t.Errorf("forcedMs = %.3f, want 4.5", summary.ForcedMs)
	}
	if len(summary.Groups) != 2 {
		t.Fatalf("groups = %+v, want 2", summary.Groups)
	}

	top := summary.Groups[0]
	if top.Frame != "measureRow@http://x/Timeline.svelte:42" {
		t.Errorf("top frame = %q", top.Frame)
	}
	if top.Count != 2 || top.Style != 1 || top.Layout != 1 {
		t.Errorf("top group = %+v, want 2 forced (1 style, 1 layout)", top)
	}
	if top.Ms != 4 {
		t.Errorf("top ms = %.3f, want 4", top.Ms)
	}
	if summary.Groups[1].Frame != "(anonymous)@http://x/b.js:7" {
		t.Errorf("second frame = %q", summary.Groups[1].Frame)
	}
}

// The stream arrives as a bare array from some Chromium builds and as
// {"traceEvents": [...]} from others. The difference is invisible until a
// parse silently reports zero forced layouts about a page full of them.
func TestParseTraceEventsAcceptsBothContainerShapes(t *testing.T) {
	wrapped := `{"metadata":{"source":"DevTools"},"traceEvents":` + traceFixtureEvents + `}`
	bare := summarizeFixture(t, traceFixtureEvents)
	object := summarizeFixture(t, wrapped)
	if bare.ForcedEvents != object.ForcedEvents || bare.ForcedMs != object.ForcedMs {
		t.Fatalf("the two container shapes disagree: %+v vs %+v", bare, object)
	}
	if len(object.Groups) != len(bare.Groups) {
		t.Fatalf("group counts differ: %d vs %d", len(object.Groups), len(bare.Groups))
	}
}

func TestParseTraceEventsRefusesAnEmptyStream(t *testing.T) {
	if _, err := parseTraceEvents([]byte("   ")); err == nil {
		t.Fatal("an empty trace must be an error, not zero events")
	}
	if _, err := parseTraceEvents([]byte("[not json")); err == nil {
		t.Fatal("an undecodable trace must be an error")
	}
}

func TestMergeTraceSummariesFoldsRepeats(t *testing.T) {
	first := summarizeFixture(t, traceFixtureEvents)
	second := summarizeFixture(t, traceFixtureEvents)
	merged := mergeTraceSummaries([]traceSummary{first, second})
	if merged == nil {
		t.Fatal("two summaries must merge into one")
	}
	if merged.ForcedEvents != 6 || merged.ForcedMs != 9 {
		t.Errorf("merged = %d events / %.1fms, want 6 / 9", merged.ForcedEvents, merged.ForcedMs)
	}
	if len(merged.Groups) != 2 {
		t.Fatalf("merged groups = %+v, want 2", merged.Groups)
	}
	if merged.Groups[0].Count != 4 {
		t.Errorf("top group count = %d, want 4", merged.Groups[0].Count)
	}
	if mergeTraceSummaries(nil) != nil {
		t.Error("no summaries must merge to nothing, not to an empty report")
	}
}

// A bench under load produces a long tail of one-off stacks. The report
// keeps the top slice and SAYS how much it dropped, because a truncated
// list that looks complete is how a reader concludes there is nothing
// else there.
func TestMergeTraceSummariesTruncatesTheTail(t *testing.T) {
	var events strings.Builder
	events.WriteString("[")
	for i := range traceTopGroups + 5 {
		if i > 0 {
			events.WriteString(",")
		}
		// Later call sites fire fewer times, so the ranking is decided by
		// count rather than by document order.
		for j := 0; j <= traceTopGroups+5-i; j++ {
			if j > 0 {
				events.WriteString(",")
			}
			events.WriteString(`{"name":"Layout","dur":1000,"args":{"beginData":{"stackTrace":[` +
				`{"functionName":"fn` + string(rune('a'+i)) + `","url":"http://x/a.js","lineNumber":1}]}}}`)
		}
	}
	events.WriteString("]")

	summary := summarizeFixture(t, events.String())
	if len(summary.Groups) != traceTopGroups+5 {
		t.Fatalf("summarize kept %d groups; it must keep all of them so a merge is not done over a truncated tail",
			len(summary.Groups))
	}
	merged := mergeTraceSummaries([]traceSummary{summary})
	if len(merged.Groups) != traceTopGroups {
		t.Errorf("merged kept %d groups, want %d", len(merged.Groups), traceTopGroups)
	}
	if merged.Truncated != 5 {
		t.Errorf("truncated = %d, want 5", merged.Truncated)
	}
	rendered := renderTraceSummary(*merged)
	if !strings.Contains(rendered, "5 further call site(s) not shown") {
		t.Errorf("the rendering hides the truncation:\n%s", rendered)
	}
	if !strings.Contains(rendered, "fna@http://x/a.js:1") {
		t.Errorf("the rendering drops the top call site:\n%s", rendered)
	}
}

func TestRenderTraceSummaryWithNothingForced(t *testing.T) {
	rendered := renderTraceSummary(traceSummary{Events: 12, ScheduledEvents: 4})
	if !strings.Contains(rendered, "no forced layout") {
		t.Errorf("a clean run must say so plainly:\n%s", rendered)
	}
}
