package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// profileFixture is a hand-built .cpuprofile shaped like the one the
// investigation this verb encodes actually produced: a write-side path
// (a wire message calling internal_set), a flush path, and — the case the
// whole split exists for — an internal_set NESTED INSIDE an effect flush.
//
//	1 (root)
//	├─ 2 handleWireMessage
//	│   └─ 3 internal_set
//	│       └─ 4 mark_reactions
//	│           └─ 5 deepMarkWork          marking
//	├─ 6 flush_queued_root_effects         flush
//	│   └─ 7 update_effect
//	│       ├─ 8 renderRow                 flush
//	│       └─ 9 internal_set              marking, UNDER a flush
//	│           └─ 10 mark_reactions       marking
//	└─ 11 (program)                        other
const profileFixture = `{
  "startTime": 1000, "endTime": 2400,
  "nodes": [
    {"id": 1,  "callFrame": {"functionName": "(root)", "url": ""}, "children": [2, 6, 11]},
    {"id": 2,  "callFrame": {"functionName": "handleWireMessage", "url": "app.js", "lineNumber": 3}, "children": [3]},
    {"id": 3,  "callFrame": {"functionName": "internal_set", "url": "svelte.js"}, "children": [4]},
    {"id": 4,  "callFrame": {"functionName": "mark_reactions", "url": "svelte.js"}, "children": [5]},
    {"id": 5,  "callFrame": {"functionName": "deepMarkWork", "url": "svelte.js"}},
    {"id": 6,  "callFrame": {"functionName": "flush_queued_root_effects", "url": "svelte.js"}, "children": [7]},
    {"id": 7,  "callFrame": {"functionName": "update_effect", "url": "svelte.js"}, "children": [8, 9]},
    {"id": 8,  "callFrame": {"functionName": "renderRow", "url": "Timeline.svelte", "lineNumber": 42}},
    {"id": 9,  "callFrame": {"functionName": "internal_set", "url": "svelte.js"}, "children": [10]},
    {"id": 10, "callFrame": {"functionName": "mark_reactions", "url": "svelte.js"}},
    {"id": 11, "callFrame": {"functionName": "(program)", "url": ""}}
  ],
  "samples":    [5,   8,   10,  11,  8,   6],
  "timeDeltas": [100, 200, 300, 400, 500, 600]
}`

func mustRollup(t *testing.T, document string) profileRollup {
	t.Helper()
	doc, err := decodeCPUProfile(json.RawMessage(document))
	if err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	return rollupCPUProfile(doc)
}

func sliceByName(t *testing.T, rollup profileRollup, name string) profileSlice {
	t.Helper()
	for _, slice := range rollup.Slices {
		if slice.Bucket == name {
			return slice
		}
	}
	t.Fatalf("rollup has no %q slice: %+v", name, rollup.Slices)
	return profileSlice{}
}

func TestRollupSplitsFlushFromMarking(t *testing.T) {
	rollup := mustRollup(t, profileFixture)

	if rollup.Samples != 6 {
		t.Errorf("samples = %d, want 6", rollup.Samples)
	}
	if rollup.UnknownSamples != 0 {
		t.Errorf("unknown samples = %d, want 0", rollup.UnknownSamples)
	}
	// 100+200+300+400+500+600 = 2100µs
	if rollup.TotalMs != 2.1 {
		t.Errorf("total = %.3fms, want 2.1ms", rollup.TotalMs)
	}

	// node 8 (200) + node 8 again (500) + node 6 sampled at its own frame
	// (600): a leaf that IS a flush frame is flush, not "other".
	flush := sliceByName(t, rollup, "svelte flush execution")
	if flush.Ms != 1.3 || flush.Samples != 3 {
		t.Errorf("flush = %+v, want 1.3ms over 3 samples", flush)
	}
	// node 5 (100, under a plain write) + node 10 (300, under an effect
	// flush). The second one is the whole point: marking membership is
	// checked before the ancestry is walked for flush, so a dirty-walk
	// inside a flush is charged to the write side.
	marking := sliceByName(t, rollup, "write-side marking")
	if marking.Ms != 0.4 || marking.Samples != 2 {
		t.Errorf("marking = %+v, want 0.4ms over 2 samples", marking)
	}
	other := sliceByName(t, rollup, "other")
	if other.Ms != 0.4 || other.Samples != 1 {
		t.Errorf("other = %+v, want 0.4ms over 1 sample", other)
	}

	var pct float64
	for _, slice := range rollup.Slices {
		pct += slice.Pct
	}
	if pct < 99.99 || pct > 100.01 {
		t.Errorf("percentages sum to %.4f, want 100", pct)
	}
}

// A sample naming a node the document does not carry must be REPORTED,
// not silently folded into "other": every percentage in the rollup is
// suspect once the two halves disagree, and a reader has to be told.
func TestRollupReportsUnknownSamples(t *testing.T) {
	rollup := mustRollup(t, `{
      "nodes": [{"id": 1, "callFrame": {"functionName": "(root)"}}],
      "samples": [1, 99], "timeDeltas": [10, 20]
    }`)
	if rollup.UnknownSamples != 1 {
		t.Fatalf("unknown samples = %d, want 1", rollup.UnknownSamples)
	}
	if !strings.Contains(renderProfileRollup(rollup, "/tmp/x.cpuprofile"), "suspect") {
		t.Error("the rendering must say the percentages are suspect")
	}
}

// V8 emits a negative leading delta on some builds. Charging it would
// subtract time from whichever bucket happened to be sampled first.
func TestRollupClampsNegativeDeltas(t *testing.T) {
	rollup := mustRollup(t, `{
      "nodes": [{"id": 1, "callFrame": {"functionName": "(root)"}}],
      "samples": [1, 1], "timeDeltas": [-5000, 1000]
    }`)
	if rollup.TotalMs != 1 {
		t.Fatalf("total = %.3fms, want 1ms", rollup.TotalMs)
	}
}

// A document with fewer deltas than samples is not an error: the samples
// still happened, and dropping them would understate what the profiler
// saw.
func TestRollupToleratesShortDeltas(t *testing.T) {
	rollup := mustRollup(t, `{
      "nodes": [{"id": 1, "callFrame": {"functionName": "flush_queued_effects"}}],
      "samples": [1, 1, 1], "timeDeltas": [1000]
    }`)
	if rollup.Samples != 3 {
		t.Errorf("samples = %d, want 3", rollup.Samples)
	}
	flush := sliceByName(t, rollup, "svelte flush execution")
	if flush.Samples != 3 || flush.Ms != 1 {
		t.Errorf("flush = %+v, want 3 samples over 1ms", flush)
	}
}

// A parent chain that loops is a broken document, and the rollup must
// answer wrongly rather than hang: an instrument that never returns is
// indistinguishable from a wedged browser.
func TestRollupSurvivesALoopedTree(t *testing.T) {
	rollup := mustRollup(t, `{
      "nodes": [
        {"id": 1, "callFrame": {"functionName": "a"}, "children": [2]},
        {"id": 2, "callFrame": {"functionName": "b"}, "children": [1]}
      ],
      "samples": [2], "timeDeltas": [1000]
    }`)
	if rollup.Samples != 1 {
		t.Fatalf("samples = %d, want 1", rollup.Samples)
	}
}

func TestDecodeCPUProfileRefusesAnEmptyDocument(t *testing.T) {
	if _, err := decodeCPUProfile(json.RawMessage(`{"samples":[]}`)); err == nil {
		t.Fatal("a profile with no nodes must be an error")
	}
}

// minifiedFixture is what a production bundle actually looks like: every
// function renamed to a letter or two, so the named split can match
// nothing — but the chunk URLs survive, and svelte-vendor visibly holds
// the time. Observed live 2026-08-26: 537/891 nodes in svelte-vendor,
// 100% "other".
const minifiedFixture = `{
  "startTime": 0, "endTime": 1000,
  "nodes": [
    {"id": 1, "callFrame": {"functionName": "(root)", "url": ""}, "children": [2, 4, 5]},
    {"id": 2, "callFrame": {"functionName": "vr", "url": "http://x/assets/svelte-vendor-abc.js"}, "children": [3]},
    {"id": 3, "callFrame": {"functionName": "B", "url": "http://x/assets/svelte-vendor-abc.js"}},
    {"id": 4, "callFrame": {"functionName": "hn", "url": "http://x/assets/index-def.js"}},
    {"id": 5, "callFrame": {"functionName": "(program)", "url": ""}}
  ],
  "samples":    [3,   3,   2,   4,   5],
  "timeDeltas": [100, 100, 100, 100, 100]
}`

// TestRollupScriptTableSurvivesMinification: against a minified build the
// named split is blind, and the rollup must say so AND still attribute
// the time somewhere — by chunk, which minification does not rename.
func TestRollupScriptTableSurvivesMinification(t *testing.T) {
	rollup := mustRollup(t, minifiedFixture)
	if !rollup.SplitBlind {
		t.Fatalf("SplitBlind = false on a minified profile with svelte-vendor time: %+v", rollup)
	}
	if len(rollup.Scripts) == 0 {
		t.Fatal("no script slices")
	}
	if rollup.Scripts[0].Script != "svelte-vendor-abc.js" {
		t.Fatalf("top script = %q, want svelte-vendor-abc.js: %+v", rollup.Scripts[0].Script, rollup.Scripts)
	}
	if rollup.Scripts[0].Samples != 3 || rollup.Scripts[0].Ms != 0.3 {
		t.Fatalf("svelte-vendor slice = %+v, want 3 samples / 0.3ms", rollup.Scripts[0])
	}
	rendered := renderProfileRollup(rollup, "x.cpuprofile")
	if !strings.Contains(rendered, "minified") || !strings.Contains(rendered, "svelte-vendor-abc.js") {
		t.Fatalf("render misses the blindness note or the script table:\n%s", rendered)
	}
}

// TestRollupScriptTableOnNamedProfile: a dev-server profile keeps the
// named split working and is NOT flagged blind; meta frames keep their
// parenthesized names in the script table.
func TestRollupScriptTableOnNamedProfile(t *testing.T) {
	rollup := mustRollup(t, profileFixture)
	if rollup.SplitBlind {
		t.Fatalf("SplitBlind on a profile whose named split matched: %+v", rollup)
	}
	found := map[string]bool{}
	for _, script := range rollup.Scripts {
		found[script.Script] = true
	}
	for _, want := range []string{"svelte.js", "(program)", "Timeline.svelte"} {
		if !found[want] {
			t.Fatalf("script table misses %q: %+v", want, rollup.Scripts)
		}
	}
}
