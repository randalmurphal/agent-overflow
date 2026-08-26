package main

// The .cpuprofile document and the one rollup this CLI computes over it.
// Pure: every function here takes a decoded profile and returns numbers,
// so the arithmetic is testable without a browser — the same split
// bench_report.go makes for the perf aggregation.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// cpuCallFrame is the subset of a V8 call frame this rollup reads. The
// document carries scriptId/column/line too; nothing here decides
// anything from them, and `--out` keeps the server's own bytes for the
// tools that do.
type cpuCallFrame struct {
	FunctionName string `json:"functionName"`
	URL          string `json:"url"`
	LineNumber   int    `json:"lineNumber"`
}

type cpuProfileNode struct {
	ID        int          `json:"id"`
	CallFrame cpuCallFrame `json:"callFrame"`
	Children  []int        `json:"children"`
}

// cpuProfileDocument is the .cpuprofile shape: a node TREE, a flat list of
// sampled node ids, and the microseconds between consecutive samples.
type cpuProfileDocument struct {
	Nodes      []cpuProfileNode `json:"nodes"`
	StartTime  int64            `json:"startTime"`
	EndTime    int64            `json:"endTime"`
	Samples    []int            `json:"samples"`
	TimeDeltas []int64          `json:"timeDeltas"`
}

// The two frame vocabularies the rollup splits on, and why they are
// different questions.
//
// FLUSH is Svelte 5 running queued effects: the scheduler entry points and
// the reaction machinery underneath them. Time under one of these is the
// framework doing the work a state change asked for.
//
// MARKING is the WRITE side: `internal_set` and the `mark_reactions` walk
// it drives. Those fire from ANY state write — an event handler, a
// WebSocket message handler, a store update — and they also fire from
// inside an effect. So marking is checked FIRST and wins wherever it is
// found in a sample's ancestry: a sample inside `mark_reactions` that
// happens to sit under an effect flush is marking cost, and folding it
// into "flush execution" would attribute the write side's dirty-walk to
// the framework's render pass, which is the exact confusion this split
// exists to prevent.
var svelteFlushFrames = map[string]struct{}{
	"flush_queued_root_effects": {},
	"flush_queued_effects":      {},
	"process_effects":           {},
	"update_effect":             {},
	"update_derived":            {},
	"execute_derived":           {},
	"update_reaction":           {},
}

var svelteMarkingFrames = map[string]struct{}{
	"internal_set":   {},
	"mark_reactions": {},
}

type profileBucket int

const (
	bucketOther profileBucket = iota
	bucketFlush
	bucketMarking
)

func (b profileBucket) label() string {
	switch b {
	case bucketFlush:
		return "svelte flush execution"
	case bucketMarking:
		return "write-side marking"
	default:
		return "other"
	}
}

// profileSlice is one bucket's share of the sampled time.
type profileSlice struct {
	Bucket  string  `json:"bucket"`
	Samples int     `json:"samples"`
	Ms      float64 `json:"ms"`
	Pct     float64 `json:"pct"`
}

// profileScriptSlice is one script's share of the sampled time, keyed by
// the URL's basename — the chunk name. This survives minification: a
// production bundle renames every function to one letter, but the chunk
// a sample sits in is still `svelte-vendor-*.js` or `index-*.js`.
type profileScriptSlice struct {
	Script  string  `json:"script"`
	Samples int     `json:"samples"`
	Ms      float64 `json:"ms"`
	Pct     float64 `json:"pct"`
}

// profileRollup is what `profile` prints after writing the document.
type profileRollup struct {
	Samples int     `json:"samples"`
	TotalMs float64 `json:"totalMs"`
	// UnknownSamples counts sample ids with no node in the tree. Reported
	// rather than silently bucketed: a non-zero count means the document is
	// inconsistent, and every percentage below it is suspect.
	UnknownSamples int            `json:"unknownSamples,omitempty"`
	Slices         []profileSlice `json:"slices"`
	// Scripts is the self-time-by-chunk table, largest first.
	Scripts []profileScriptSlice `json:"scripts,omitempty"`
	// SplitBlind is set when the named flush/marking split matched NOTHING
	// but svelte-vendor time is visibly there in the script table: the
	// build is minified and the named split cannot see into it. Without
	// this flag a 0% svelte row reads as "svelte is free", which is the
	// opposite of what it means.
	SplitBlind bool `json:"splitBlind,omitempty"`
}

func decodeCPUProfile(raw json.RawMessage) (cpuProfileDocument, error) {
	var doc cpuProfileDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return cpuProfileDocument{}, fmt.Errorf("decode cpu profile: %w", err)
	}
	if len(doc.Nodes) == 0 {
		return cpuProfileDocument{}, fmt.Errorf("the profile carries no nodes (was the profiler running?)")
	}
	return doc, nil
}

// rollupCPUProfile splits the sampled time three ways.
//
// timeDeltas[i] is the gap BEFORE samples[i], so it is charged to
// samples[i] — the standard reading, and the one every .cpuprofile viewer
// uses. A document whose two arrays disagree in length is not an error:
// a sample with no delta contributes zero time and is still counted, so
// the sample counts stay honest about what the profiler saw.
func rollupCPUProfile(doc cpuProfileDocument) profileRollup {
	byID := make(map[int]cpuProfileNode, len(doc.Nodes))
	parent := make(map[int]int, len(doc.Nodes))
	for _, node := range doc.Nodes {
		byID[node.ID] = node
	}
	for _, node := range doc.Nodes {
		for _, child := range node.Children {
			parent[child] = node.ID
		}
	}

	buckets := make(map[int]profileBucket, len(doc.Nodes))
	classify := func(id int) profileBucket {
		return classifyNode(id, byID, parent, buckets)
	}

	rollup := profileRollup{Samples: len(doc.Samples)}
	samples := map[profileBucket]int{}
	micros := map[profileBucket]int64{}
	scriptSamples := map[string]int{}
	scriptMicros := map[string]int64{}
	for i, id := range doc.Samples {
		var delta int64
		if i < len(doc.TimeDeltas) {
			delta = doc.TimeDeltas[i]
		}
		if delta < 0 {
			// V8 emits a negative first delta on some builds (the gap before
			// the profile's own start). Charging it would subtract time from
			// whichever bucket happened to be first.
			delta = 0
		}
		node, ok := byID[id]
		if !ok {
			rollup.UnknownSamples++
		}
		bucket := classify(id)
		samples[bucket]++
		micros[bucket] += delta
		script := scriptKey(node.CallFrame)
		scriptSamples[script]++
		scriptMicros[script] += delta
	}

	total := micros[bucketFlush] + micros[bucketMarking] + micros[bucketOther]
	rollup.TotalMs = float64(total) / 1000
	for _, bucket := range []profileBucket{bucketFlush, bucketMarking, bucketOther} {
		slice := profileSlice{
			Bucket:  bucket.label(),
			Samples: samples[bucket],
			Ms:      float64(micros[bucket]) / 1000,
		}
		if total > 0 {
			slice.Pct = 100 * float64(micros[bucket]) / float64(total)
		}
		rollup.Slices = append(rollup.Slices, slice)
	}
	rollup.Scripts = topScripts(scriptSamples, scriptMicros, total)
	if samples[bucketFlush] == 0 && samples[bucketMarking] == 0 {
		for _, script := range rollup.Scripts {
			if strings.HasPrefix(script.Script, "svelte-vendor") && script.Samples > 0 {
				rollup.SplitBlind = true
				break
			}
		}
	}
	return rollup
}

// scriptKey names the chunk a frame belongs to. V8's meta frames
// ((program), (idle), (garbage collector)) carry no URL but a
// parenthesized name worth keeping; everything else URL-less is native
// or anonymous glue.
func scriptKey(frame cpuCallFrame) string {
	if frame.URL != "" {
		if i := strings.LastIndexByte(frame.URL, '/'); i >= 0 {
			return frame.URL[i+1:]
		}
		return frame.URL
	}
	if strings.HasPrefix(frame.FunctionName, "(") {
		return frame.FunctionName
	}
	return "(no script)"
}

// maxProfileScripts bounds the printed chunk table. The point is "where
// did the time go", and past a handful of rows the answer is noise.
const maxProfileScripts = 8

func topScripts(samples map[string]int, micros map[string]int64, total int64) []profileScriptSlice {
	slices := make([]profileScriptSlice, 0, len(samples))
	for script, count := range samples {
		slice := profileScriptSlice{
			Script:  script,
			Samples: count,
			Ms:      float64(micros[script]) / 1000,
		}
		if total > 0 {
			slice.Pct = 100 * float64(micros[script]) / float64(total)
		}
		slices = append(slices, slice)
	}
	sort.Slice(slices, func(i, j int) bool {
		if slices[i].Ms != slices[j].Ms {
			return slices[i].Ms > slices[j].Ms
		}
		if slices[i].Samples != slices[j].Samples {
			return slices[i].Samples > slices[j].Samples
		}
		return slices[i].Script < slices[j].Script
	})
	if len(slices) > maxProfileScripts {
		slices = slices[:maxProfileScripts]
	}
	return slices
}

// maxProfileDepth bounds the ancestry walk. A .cpuprofile is a tree, but
// it arrives over a wire: a malformed document whose parent chain loops
// would hang the rollup rather than mis-report it, which is the worse
// failure of the two.
const maxProfileDepth = 8192

// classifyNode resolves one node's bucket, memoized. The recursion is
// what encodes the precedence: a node inherits its parent's bucket unless
// its OWN frame is a stronger one, and marking is stronger than flush
// wherever the two meet.
func classifyNode(id int, byID map[int]cpuProfileNode, parent map[int]int, memo map[int]profileBucket) profileBucket {
	if bucket, ok := memo[id]; ok {
		return bucket
	}
	// Collect the chain root-ward, stopping at the first node already
	// resolved, then fill the memo downward.
	chain := make([]int, 0, 16)
	current := id
	inherited := bucketOther
	seen := map[int]struct{}{}
	for depth := 0; depth < maxProfileDepth; depth++ {
		if bucket, ok := memo[current]; ok {
			inherited = bucket
			break
		}
		if _, looped := seen[current]; looped {
			break
		}
		seen[current] = struct{}{}
		chain = append(chain, current)
		next, ok := parent[current]
		if !ok {
			break
		}
		current = next
	}
	for i := len(chain) - 1; i >= 0; i-- {
		node := byID[chain[i]]
		bucket := inherited
		name := node.CallFrame.FunctionName
		if _, ok := svelteMarkingFrames[name]; ok {
			bucket = bucketMarking
		} else if _, ok := svelteFlushFrames[name]; ok && bucket != bucketMarking {
			bucket = bucketFlush
		}
		memo[chain[i]] = bucket
		inherited = bucket
	}
	return memo[id]
}

func renderProfileRollup(rollup profileRollup, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "profile: %d samples over %.0fms\n", rollup.Samples, rollup.TotalMs)
	rows := make([][]string, 0, len(rollup.Slices))
	for _, slice := range rollup.Slices {
		rows = append(rows, []string{
			"  " + slice.Bucket,
			fmt.Sprintf("%.1fms", slice.Ms),
			fmt.Sprintf("%.1f%%", slice.Pct),
			fmt.Sprintf("%d samples", slice.Samples),
		})
	}
	b.WriteString(tableString(nil, rows))
	if rollup.UnknownSamples > 0 {
		fmt.Fprintf(&b, "  %d sample(s) name a node the profile does not carry — the percentages above are suspect\n",
			rollup.UnknownSamples)
	}
	if rollup.SplitBlind {
		b.WriteString("  NOTE: no named svelte frames matched, but svelte-vendor time is in the script table —\n")
		b.WriteString("  this build is minified, so the flush/marking split cannot see into it.\n")
		b.WriteString("  For the named split, profile an instance serving the dev server (make dev / make harness with FRONTEND_DEVSERVER_URL).\n")
	}
	if len(rollup.Scripts) > 0 {
		b.WriteString("\nby script (self time):\n")
		scriptRows := make([][]string, 0, len(rollup.Scripts))
		for _, script := range rollup.Scripts {
			scriptRows = append(scriptRows, []string{
				"  " + script.Script,
				fmt.Sprintf("%.1fms", script.Ms),
				fmt.Sprintf("%.1f%%", script.Pct),
				fmt.Sprintf("%d samples", script.Samples),
			})
		}
		b.WriteString(tableString(nil, scriptRows))
	}
	fmt.Fprintf(&b, "\nprofile: %s\n", path)
	return b.String()
}
