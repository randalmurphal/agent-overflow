package highlight

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// Result is one highlight run's output: per-line encoded spans plus
// whether the input exceeded a size cap (lines past a cap are plain).
// Lines may be SHORTER than the input's line count — absent trailing
// entries are plain, and every renderer treats a missing index as a
// plain line — so degradation paths (oversized input, unknown
// language, timeout, malformed input) yield plain spans, never an
// error, and never allocate proportional to unbounded input.
type Result struct {
	Lines     []EncodedLine
	Truncated bool

	// Incomplete marks a result that degraded because a parse FAILED
	// (timeout, parser error, patch budget exhaustion) rather than
	// because of an input cap. Failed parses can succeed on retry, so
	// incomplete results must not be memoized (see Cache.get) — and the
	// flag crosses the RPC boundary so frontend caches can apply the
	// same transient-vs-permanent distinction.
	Incomplete bool
}

// Highlight parses src as lang and returns per-line spans. src is the
// exact text whose lines the caller will render: run byte-lengths per
// result line sum to that line's byte length.
func Highlight(lang Lang, src []byte) Result {
	if len(src) > MaxRequestBytes {
		// The parse head is capped far below this; don't count or
		// allocate proportional to the input. Nil lines = all plain.
		return Result{Truncated: true}
	}
	eng := engineFor(lang)
	if eng == nil {
		return Result{}
	}
	total := countLines(src)

	head := src
	truncated := false
	if len(src) > maxInputBytes {
		cut := bytes.LastIndexByte(src[:maxInputBytes], '\n')
		if cut < 0 {
			// A single over-cap line renders plain anyway.
			return Result{Truncated: true}
		}
		head = src[:cut]
		truncated = true
	}
	if countLines(head) > maxResultLines {
		// Sub-4-byte average line length is not code; parsing it would
		// allocate result lines far past any legitimate document.
		return Result{Truncated: true}
	}

	classes, complete := eng.classify(head)
	if classes == nil {
		return Result{Truncated: truncated, Incomplete: true}
	}
	lines := encodeLines(head, classes)
	for len(lines) < total && len(lines) < maxResultLines {
		lines = append(lines, EncodedLine{})
	}
	return Result{Lines: lines, Truncated: truncated || total > maxResultLines, Incomplete: !complete}
}

// HighlightPatchText highlights one file's unified diff. The result is
// patch-aligned: Lines[i] corresponds to patch line i as the frontend
// splits it (meta/@@/marker lines plain), so the consumer indexes by
// PatchLine position with zero bookkeeping. Each hunk's old side
// (context+del) and new side (context+add) parse as whole virtual
// documents, which is what makes multi-line constructs come out right.
func HighlightPatchText(lang Lang, patch string) Result {
	return HighlightPatchTextPrimed(lang, patch, "")
}

// HighlightPatchTextPrimed is HighlightPatchText with the new-side
// file content spliced around each hunk's virtual documents as
// parse-priming context: the content ABOVE the hunk is prepended (a
// hunk that starts mid-construct — inside a docstring, block comment,
// template literal — still highlights correctly) and the content BELOW
// it is appended (a construct the hunk sits inside still CLOSES — a
// hunk inside a raw-text element like svelte/html `<script>` needs the
// closing tag to exist before the grammar emits the node its language
// injection anchors on; without the suffix such hunks paint fully
// plain). Above the first hunk the old and new files are identical, so
// priming both sides with new-side text is exact there; for later
// hunks and for the suffix it is a best-effort approximation that the
// unprimed path gets wrong anyway. Empty fileContent means no priming.
func HighlightPatchTextPrimed(lang Lang, patch, fileContent string) Result {
	if len(patch) > MaxRequestBytes {
		return Result{Truncated: true}
	}
	parsed := parsePatch(patch)
	if parsed.lineCount > maxResultLines {
		return Result{Truncated: true}
	}
	out := plainLines(parsed.lineCount)
	eng := engineFor(lang)
	if eng == nil || len(parsed.hunks) == 0 {
		return Result{Lines: out}
	}

	budget := time.Now().Add(patchParseBudget)
	hunkNeedOld := make([]bool, len(parsed.hunks))
	hunkNeedNew := make([]bool, len(parsed.hunks))
	anyOld, anyNew := false, false
	for i := range parsed.hunks {
		for _, ref := range parsed.hunks[i].lines {
			switch ref.side {
			case sideOld:
				hunkNeedOld[i] = true
				anyOld = true
			case sideNew, sideBoth:
				hunkNeedNew[i] = true
				anyNew = true
			case sideNone:
			}
		}
	}
	if !anyOld && !anyNew {
		return Result{Lines: out}
	}

	// Fast path: when primed with fileContent and hunks are ascending and
	// non-overlapping, reconstruct one whole old-side file and one whole
	// new-side file (at most 2 tree-sitter parses regardless of hunk count)
	// and run query captures + RLE encoding strictly on each hunk's byte
	// range. Falls back to per-hunk trimmed windows if a reconstructed file
	// exceeds maxInputBytes or if hunks are out of order / past EOF.
	if fileContent != "" {
		if oldLinesByHunk, newLinesByHunk, res, ok := highlightPatchWholeDocs(eng, fileContent, parsed.hunks, hunkNeedOld, hunkNeedNew, anyOld, anyNew, budget); ok {
			for i, hunk := range parsed.hunks {
				var oldLines, newLines []EncodedLine
				if i < len(oldLinesByHunk) {
					oldLines = oldLinesByHunk[i]
				}
				if i < len(newLinesByHunk) {
					newLines = newLinesByHunk[i]
				}
				applyHunkLines(out, hunk, oldLines, newLines)
			}
			return Result{Lines: out, Truncated: res.Truncated, Incomplete: res.Incomplete}
		}
	}

	truncated := false
	incomplete := false
	primer := primer{content: fileContent}
	var memo hunkDocMemo
	if fileContent != "" && len(fileContent) <= maxInputBytes {
		memo = make(hunkDocMemo, 2)
	}

	for i, hunk := range parsed.hunks {
		if time.Now().After(budget) {
			// Aggregate budget spent — remaining hunks render plain.
			// Wall-clock exhaustion is load-dependent (the same patch
			// can complete on an idle retry), so like a parse timeout
			// the partial result must not be memoized.
			truncated = true
			incomplete = true
			break
		}
		prime := primer.primeFor(hunk.newStart)
		suffix := primer.suffixFrom(hunk.newStart + hunk.newCount)
		var oldLines, newLines []EncodedLine
		var oldRes, newRes Result
		if hunkNeedOld[i] {
			oldLines, oldRes = highlightHunkDoc(eng, lang, prime, suffix, hunk.oldDoc, memo)
		}
		if hunkNeedNew[i] {
			newLines, newRes = highlightHunkDoc(eng, lang, prime, suffix, hunk.newDoc, memo)
		}
		truncated = truncated || oldRes.Truncated || newRes.Truncated
		incomplete = incomplete || oldRes.Incomplete || newRes.Incomplete
		applyHunkLines(out, hunk, oldLines, newLines)
	}
	return Result{Lines: out, Truncated: truncated, Incomplete: incomplete}
}

func applyHunkLines(out []EncodedLine, hunk patchHunk, oldLines, newLines []EncodedLine) {
	for _, ref := range hunk.lines {
		var line EncodedLine
		switch ref.side {
		case sideOld:
			if ref.oldDocLine < len(oldLines) {
				line = oldLines[ref.oldDocLine]
			}
		case sideNew, sideBoth:
			if ref.newDocLine < len(newLines) {
				line = newLines[ref.newDocLine]
			}
		case sideNone:
		}
		out[ref.patchIndex] = padRuns(line, ref.outPad)
	}
}

type byteRange struct {
	lo, hi int
}

// buildWholePrimedDoc splices all hunks' old or new docs into fileContent in a
// single forward pass, returning the reconstructed file bytes, each hunk's
// content byte range inside it, and each hunk's splice range (the exact region
// between identical unchanged gap segments, for incremental tree-sitter edits).
// Returns ok=false if hunks are out of order, overlap, extend past
// fileContent, or exceed maxInputBytes / maxResultLines.
func buildWholePrimedDoc(fileContent string, hunks []patchHunk, useOld bool, needHunk []bool) (doc []byte, ranges []byteRange, splices []byteRange, ok bool) {
	// Validate ascending non-overlapping hunk order first.
	prevEnd := 1
	totalHunkBytes := 0
	for _, h := range hunks {
		if h.newStart < prevEnd {
			return nil, nil, nil, false
		}
		prevEnd = h.newStart + h.newCount
		if useOld {
			totalHunkBytes += len(h.oldDoc) + 1
		} else {
			totalHunkBytes += len(h.newDoc) + 1
		}
	}

	ranges = make([]byteRange, len(hunks))
	splices = make([]byteRange, len(hunks))
	buf := make([]byte, 0, len(fileContent)+totalHunkBytes)
	hasContent := false
	line := 1 // 1-based current line in fileContent
	off := 0  // byte offset of `line` in fileContent

	advanceTo := func(targetLine int) (gap string, hadGap bool, pastEOF bool) {
		if targetLine <= line {
			return "", false, false
		}
		startOff := off
		for line < targetLine {
			if off > len(fileContent) {
				return "", false, true
			}
			idx := strings.IndexByte(fileContent[off:], '\n')
			if idx < 0 {
				return "", false, true
			}
			off += idx + 1
			line++
		}
		// Exclude the newline immediately before targetLine.
		return fileContent[startOff : off-1], true, false
	}

	skipTo := func(targetLine int) bool {
		for line < targetLine {
			if off >= len(fileContent) {
				// Last line without trailing newline: advancing past it lands at EOF.
				if line+1 == targetLine {
					off = len(fileContent)
					line++
					return true
				}
				return false
			}
			idx := strings.IndexByte(fileContent[off:], '\n')
			if idx < 0 {
				if line+1 == targetLine {
					off = len(fileContent)
					line++
					return true
				}
				return false
			}
			off += idx + 1
			line++
		}
		return true
	}

	// appendSegment returns the start offset of seg's own bytes (after any
	// separating '\n') and the end offset after seg.
	appendSegment := func(seg string) (segStart, segEnd int) {
		if hasContent {
			buf = append(buf, '\n')
		}
		segStart = len(buf)
		buf = append(buf, seg...)
		segEnd = len(buf)
		hasContent = true
		return segStart, segEnd
	}

	for i, h := range hunks {
		gap, hadGap, pastEOF := advanceTo(h.newStart)
		if pastEOF {
			return nil, nil, nil, false
		}
		gapStart := len(buf)
		if hadGap {
			gapStart, _ = appendSegment(gap)
		}
		if i > 0 {
			splices[i-1].hi = gapStart
		}
		splices[i].lo = len(buf)
		var hunkBytes []byte
		if useOld {
			hunkBytes = h.oldDoc
		} else {
			hunkBytes = h.newDoc
		}
		if len(hunkBytes) > 0 {
			if hasContent {
				buf = append(buf, '\n')
			}
			lo := len(buf)
			buf = append(buf, hunkBytes...)
			hi := len(buf)
			hasContent = true
			if needHunk[i] {
				ranges[i] = byteRange{lo: lo, hi: hi}
			}
		}
		if !skipTo(h.newStart + h.newCount) {
			return nil, nil, nil, false
		}
	}

	tailStart := len(buf)
	if off < len(fileContent) {
		tailStart, _ = appendSegment(fileContent[off:])
	}
	if len(hunks) > 0 {
		splices[len(hunks)-1].hi = tailStart
	}

	if len(buf) > maxInputBytes || countLines(buf) > maxResultLines {
		return nil, nil, nil, false
	}
	return buf, ranges, splices, true
}

func advancePoint(start tree_sitter.Point, seg []byte) tree_sitter.Point {
	if len(seg) == 0 {
		return start
	}
	lastNL := bytes.LastIndexByte(seg, '\n')
	if lastNL < 0 {
		return tree_sitter.Point{
			Row:    start.Row,
			Column: start.Column + uint(len(seg)),
		}
	}
	rows := bytes.Count(seg[:lastNL+1], []byte{'\n'})
	return tree_sitter.Point{
		Row:    start.Row + uint(rows),
		Column: uint(len(seg) - lastNL - 1),
	}
}

// applyIncrementalEdits mutates oldTree in-place so tree-sitter can parse
// newDoc incrementally from oldTree, reusing unchanged AST subtrees across
// unmodified gaps between hunks. Returns false if splice geometry is not
// edit-compatible, signaling the caller to parse newDoc from scratch.
func applyIncrementalEdits(
	oldTree *tree_sitter.Tree,
	oldDoc []byte,
	oldSplices []byteRange,
	newDoc []byte,
	newSplices []byteRange,
) bool {
	if oldTree == nil || len(oldSplices) != len(newSplices) || len(oldSplices) == 0 {
		return false
	}
	expectedNewLen := len(oldDoc)
	for i := range oldSplices {
		os, ns := oldSplices[i], newSplices[i]
		if os.lo < 0 || os.hi < os.lo || os.hi > len(oldDoc) || ns.lo < 0 || ns.hi < ns.lo || ns.hi > len(newDoc) {
			return false
		}
		expectedNewLen += (ns.hi - ns.lo) - (os.hi - os.lo)
	}
	if expectedNewLen != len(newDoc) {
		return false
	}

	// Single forward pass over oldDoc to compute (Row, Column) points for each
	// splice start/end. Applying edits in reverse order below keeps every
	// earlier splice's old byte offset and point coordinates invariant.
	oldStartPts := make([]tree_sitter.Point, len(oldSplices))
	oldEndPts := make([]tree_sitter.Point, len(oldSplices))
	pos := 0
	pt := tree_sitter.Point{}
	for i, os := range oldSplices {
		if os.lo < pos {
			return false
		}
		pt = advancePoint(pt, oldDoc[pos:os.lo])
		pos = os.lo
		oldStartPts[i] = pt
		pt = advancePoint(pt, oldDoc[pos:os.hi])
		pos = os.hi
		oldEndPts[i] = pt
	}

	for i := len(oldSplices) - 1; i >= 0; i-- {
		os, ns := oldSplices[i], newSplices[i]
		oldSeg := oldDoc[os.lo:os.hi]
		newSeg := newDoc[ns.lo:ns.hi]
		if bytes.Equal(oldSeg, newSeg) {
			continue
		}
		oldTree.Edit(&tree_sitter.InputEdit{
			StartByte:      uint(os.lo),
			OldEndByte:     uint(os.hi),
			NewEndByte:     uint(os.lo + len(newSeg)),
			StartPosition:  oldStartPts[i],
			OldEndPosition: oldEndPts[i],
			NewEndPosition: advancePoint(oldStartPts[i], newSeg),
		})
	}
	return true
}

func highlightPatchWholeDocs(
	eng *engine,
	fileContent string,
	hunks []patchHunk,
	needOld, needNew []bool,
	anyOld, anyNew bool,
	budget time.Time,
) (oldByHunk, newByHunk [][]EncodedLine, res Result, ok bool) {
	var oldDoc, newDoc []byte
	var oldRanges, newRanges []byteRange
	var oldSplices, newSplices []byteRange

	if anyOld {
		oldDoc, oldRanges, oldSplices, ok = buildWholePrimedDoc(fileContent, hunks, true, needOld)
		if !ok {
			return nil, nil, Result{}, false
		}
	}
	if anyNew {
		newDoc, newRanges, newSplices, ok = buildWholePrimedDoc(fileContent, hunks, false, needNew)
		if !ok {
			return nil, nil, Result{}, false
		}
	}

	var incomplete bool
	var oldTree *tree_sitter.Tree
	keepOldTree := anyOld && anyNew && len(oldDoc) > 0 && len(newDoc) > 0
	defer func() {
		if oldTree != nil {
			oldTree.Close()
		}
	}()

	if anyOld && len(oldDoc) > 0 {
		if time.Now().After(budget) {
			return nil, nil, Result{Truncated: true, Incomplete: true}, true
		}
		hunkDocParses.Add(1)
		var lines [][]EncodedLine
		var complete bool
		lines, complete, oldTree = eng.highlightRangesWithTree(oldDoc, oldRanges, nil, keepOldTree)
		if lines == nil {
			incomplete = true
		} else {
			oldByHunk = lines
			incomplete = incomplete || !complete
		}
	}
	if anyNew && len(newDoc) > 0 {
		if time.Now().After(budget) {
			return oldByHunk, nil, Result{Truncated: true, Incomplete: true}, true
		}
		var reuseTree *tree_sitter.Tree
		if oldTree != nil && applyIncrementalEdits(oldTree, oldDoc, oldSplices, newDoc, newSplices) {
			reuseTree = oldTree
		}
		hunkDocParses.Add(1)
		lines, complete, _ := eng.highlightRangesWithTree(newDoc, newRanges, reuseTree, false)
		if lines == nil {
			incomplete = true
		} else {
			newByHunk = lines
			incomplete = incomplete || !complete
		}
	}
	return oldByHunk, newByHunk, Result{Incomplete: incomplete}, true
}

// primer yields fileContent prefixes (everything above a hunk's first
// line) and suffixes (everything below its last line) as zero-copy
// slices. Hunks arrive in ascending newStart order and each hunk asks
// prefix-then-suffix, so one forward newline scan serves the whole
// patch; a malformed descending header restarts the scan.
type primer struct {
	content string
	line    int // 0-based count of newlines consumed up to off
	off     int
}

// primeFor returns the content above 1-based file line newStart —
// byte-equal to strings.Join(strings.Split(content,"\n")[:newStart-1],
// "\n"), without materializing either.
func (pr *primer) primeFor(newStart int) string {
	n := newStart - 1
	if pr.content == "" || n <= 0 {
		return ""
	}
	if n < pr.line {
		pr.line, pr.off = 0, 0
	}
	for pr.line < n {
		next := strings.IndexByte(pr.content[pr.off:], '\n')
		if next < 0 {
			// Fewer lines than requested: the whole content primes.
			return pr.content
		}
		pr.off += next + 1
		pr.line++
	}
	// off sits just past the (n)th newline; the prime excludes it.
	return pr.content[:pr.off-1]
}

// suffixFrom returns the content from 1-based file line fileLine to
// the end — byte-equal to strings.Join(strings.Split(content, "\n")
// [fileLine-1:], "\n"). Empty when the content has fewer lines.
func (pr *primer) suffixFrom(fileLine int) string {
	n := fileLine - 1
	if pr.content == "" {
		return ""
	}
	if n <= 0 {
		return pr.content
	}
	if n < pr.line {
		pr.line, pr.off = 0, 0
	}
	for pr.line < n {
		next := strings.IndexByte(pr.content[pr.off:], '\n')
		if next < 0 {
			return ""
		}
		pr.off += next + 1
		pr.line++
	}
	return pr.content[pr.off:]
}

// hunkDocMemo caches spliced-document Highlight results within one
// HighlightPatchTextPrimed call, keyed by the exact bytes the splice
// would parse. Incomplete results are never stored — the same
// transient-degradation rule the Cache applies, scoped to this call.
type hunkDocMemo map[[sha256.Size]byte]Result

// hunkDocParses counts whole-document parses issued by
// highlightHunkDoc. Test hook: the memo regression test pins that an
// H-hunk matching patch parses its identical new-side splices once,
// not H times.
var hunkDocParses atomic.Int64

func highlightHunkParse(lang Lang, src []byte) Result {
	hunkDocParses.Add(1)
	return Highlight(lang, src)
}

// highlightHunkDoc highlights one reconstructed hunk side, optionally
// spliced between preceding and following file content. Empty docs
// (the old side of an added file) short-circuit; only the doc's own
// lines are returned.
func highlightHunkDoc(eng *engine, lang Lang, prime, suffix string, doc []byte, memo hunkDocMemo) ([]EncodedLine, Result) {
	if len(doc) == 0 {
		return nil, Result{}
	}
	if prime == "" && suffix == "" {
		res := highlightHunkParse(lang, doc)
		return res.Lines, res
	}
	// Keep the combined document under the input cap: first trim the
	// suffix's TAIL (the text nearest the hunk closes the constructs
	// that matter), then the prime's HEAD (same reasoning, other
	// direction). If the doc alone is over cap, priming is pointless —
	// let Highlight's own truncation handle it.
	if overflow := len(prime) + 1 + len(doc) + len(suffix) + 1 - maxInputBytes; overflow > 0 && suffix != "" {
		keep := len(suffix) - overflow
		if keep <= 0 {
			suffix = ""
		} else if cut := strings.LastIndexByte(suffix[:keep], '\n'); cut > 0 {
			suffix = suffix[:cut]
		} else {
			suffix = ""
		}
	}
	if overflow := len(prime) + 1 + len(doc) - maxInputBytes; overflow > 0 {
		if overflow >= len(prime) {
			res := highlightHunkParse(lang, doc)
			return res.Lines, res
		}
		cut := strings.IndexByte(prime[overflow:], '\n')
		if cut < 0 {
			res := highlightHunkParse(lang, doc)
			return res.Lines, res
		}
		prime = prime[overflow+cut+1:]
	}
	// Memo lookup before materializing the splice: hash the exact byte
	// sequence the concat below would produce.
	var key [sha256.Size]byte
	if memo != nil {
		h := sha256.New()
		if prime != "" {
			io.WriteString(h, prime)
			h.Write([]byte{'\n'})
		}
		h.Write(doc)
		if suffix != "" {
			h.Write([]byte{'\n'})
			io.WriteString(h, suffix)
		}
		h.Sum(key[:0])
		if res, ok := memo[key]; ok {
			return sliceHunkDocLines(res, prime, doc), res
		}
	}
	size := len(prime) + 1 + len(doc)
	if suffix != "" {
		size += 1 + len(suffix)
	}
	combined := make([]byte, 0, size)
	docLo := 0
	if prime != "" {
		combined = append(combined, prime...)
		combined = append(combined, '\n')
		docLo = len(combined)
	}
	combined = append(combined, doc...)
	docHi := len(combined)
	if suffix != "" {
		combined = append(combined, '\n')
		combined = append(combined, suffix...)
	}
	// When no memo needs the full document (or combined fits caps), query and
	// encode only [docLo, docHi] instead of the whole 1 MiB prime+suffix window.
	if memo == nil && len(combined) <= maxInputBytes && countLines(combined) <= maxResultLines {
		hunkDocParses.Add(1)
		linesByRange, complete := eng.highlightRanges(combined, []byteRange{{lo: docLo, hi: docHi}})
		if linesByRange == nil {
			return nil, Result{Incomplete: true}
		}
		return linesByRange[0], Result{Lines: linesByRange[0], Incomplete: !complete}
	}
	res := highlightHunkParse(lang, combined)
	if memo != nil && !res.Incomplete {
		memo[key] = res
	}
	return sliceHunkDocLines(res, prime, doc), res
}

// sliceHunkDocLines cuts the doc's own lines out of a spliced-document
// result: skip the prime's lines, drop the suffix's trailing entries —
// callers index doc lines only, and the trimmed slice keeps that
// contract visible. Shared lines are safe across hunks: padRuns copies
// before padding and renderers never mutate.
func sliceHunkDocLines(res Result, prime string, doc []byte) []EncodedLine {
	skip := 0
	if prime != "" {
		// strings.Count avoids the []byte(prime) copy countLines would
		// take — the prime is most of the file on every hunk.
		skip = strings.Count(prime, "\n") + 1
	}
	if skip >= len(res.Lines) {
		return nil
	}
	docLines := countLines(doc)
	lines := res.Lines[skip:]
	if len(lines) > docLines {
		lines = lines[:docLines]
	}
	return lines
}

// paintSpan is one capture's byte range awaiting precedence
// resolution.
type paintSpan struct {
	start, end int
	class      uint16
	pattern    uint
}

// classify parses src and returns a per-byte class buffer, or nil when
// parsing failed (timeout) and the caller should degrade to plain.
// complete reports whether every parse — including injected
// sub-parses — succeeded; incomplete results must not be memoized.
func (e *engine) classify(src []byte) (classes []uint16, complete bool) {
	classes = make([]uint16, len(src))
	ok, complete := e.paint(src, nil, classes, 0)
	if !ok {
		return nil, false
	}
	return classes, complete
}

// highlightRanges parses src once, runs capture queries and injections only
// over the non-empty byte ranges in targets, and returns one []EncodedLine
// slice per entry in targets (nil for empty ranges). Returns nil on parse
// timeout/failure.
func (e *engine) highlightRanges(src []byte, targets []byteRange) ([][]EncodedLine, bool) {
	lines, complete, _ := e.highlightRangesWithTree(src, targets, nil, false)
	return lines, complete
}

func (e *engine) highlightRangesWithTree(
	src []byte,
	targets []byteRange,
	oldTree *tree_sitter.Tree,
	returnTree bool,
) ([][]EncodedLine, bool, *tree_sitter.Tree) {
	totalBytes := 0
	for _, r := range targets {
		if r.hi > r.lo {
			totalBytes += r.hi - r.lo
		}
	}
	if totalBytes == 0 {
		return make([][]EncodedLine, len(targets)), true, nil
	}
	slabPtr, slab := acquireClassSlab(totalBytes)
	defer releaseClassSlab(slabPtr, slab)

	slicesBuf := make([][]uint16, len(targets))
	pos := 0
	for i, r := range targets {
		if r.hi > r.lo {
			n := r.hi - r.lo
			slicesBuf[i] = slab[pos : pos+n]
			pos += n
		}
	}

	ok, complete, tree := e.paintTargetRanges(src, nil, targets, slicesBuf, 0, oldTree, returnTree)
	if !ok {
		return nil, false, nil
	}

	out := make([][]EncodedLine, len(targets))
	for i, r := range targets {
		if r.hi > r.lo {
			out[i] = encodeLines(src[r.lo:r.hi], slicesBuf[i])
		}
	}
	return out, complete, tree
}

// injectionOverlapsTargets reports whether any range in site overlaps any
// non-empty target range. Both inputs are sorted ascending.
func injectionOverlapsTargets(site []tree_sitter.Range, targets []byteRange) bool {
	for _, sr := range site {
		sLo, sHi := int(sr.StartByte), int(sr.EndByte)
		for _, tr := range targets {
			if tr.hi <= tr.lo {
				continue
			}
			if tr.lo >= sHi {
				break
			}
			if tr.hi > sLo {
				return true
			}
		}
	}
	return false
}

func (e *engine) paintTargetRanges(
	src []byte,
	included []tree_sitter.Range,
	targets []byteRange,
	classSlices [][]uint16,
	depth int,
	oldTree *tree_sitter.Tree,
	returnTree bool,
) (ok, complete bool, keptTree *tree_sitter.Tree) {
	p := acquireParser()
	if err := p.SetLanguage(e.lang); err != nil {
		releaseParser(p)
		return false, false, nil
	}
	if included != nil {
		if err := p.SetIncludedRanges(included); err != nil {
			releaseParser(p)
			return false, false, nil
		}
	}
	tree := parseWithDeadline(p, src, oldTree)
	if tree == nil {
		p.Close()
		return false, false, nil
	}
	releaseParser(p)
	defer func() {
		if keptTree == nil {
			tree.Close()
		}
	}()

	qc := acquireQueryCursor()
	spansPtr := paintSpanPool.Get().(*[]paintSpan)
	spans := (*spansPtr)[:0]
	for i, r := range targets {
		if r.hi <= r.lo {
			continue
		}
		if included != nil && !injectionOverlapsTargets(included, targets[i:i+1]) {
			continue
		}
		spans = e.paintCapturesWith(qc, spans[:0], tree, src, r.lo, r.hi, classSlices[i])
	}
	if cap(spans) <= maxRetainedPaintSpanCap {
		*spansPtr = spans[:0]
		paintSpanPool.Put(spansPtr)
	}
	releaseQueryCursor(qc)

	complete = true
	if e.inj != nil && depth < maxInjectionDepth {
		for _, site := range e.inj.collectInjectionsInRanges(tree.RootNode(), src, targets) {
			if !injectionOverlapsTargets(site.ranges, targets) {
				continue
			}
			if child := engineFor(site.lang); child != nil {
				_, childComplete, _ := child.paintTargetRanges(src, site.ranges, targets, classSlices, depth+1, nil, false)
				complete = complete && childComplete
			}
		}
	}
	if returnTree {
		keptTree = tree
	}
	return true, complete, keptTree
}

// paint parses src — restricted to ranges when non-nil (injection
// regions, host-document coordinates) — paints capture classes into
// classes, and recurses into injections. ok reports whether THIS parse
// succeeded (a failed injected parse just leaves its region as the
// host painted it); complete reports whether this parse and every
// descendant succeeded.
func (e *engine) paint(src []byte, ranges []tree_sitter.Range, classes []uint16, depth int) (ok, complete bool) {
	p := acquireParser()
	if err := p.SetLanguage(e.lang); err != nil {
		releaseParser(p)
		return false, false
	}
	if ranges != nil {
		if err := p.SetIncludedRanges(ranges); err != nil {
			releaseParser(p)
			return false, false
		}
	}
	tree := parseWithDeadline(p, src, nil)
	if tree == nil {
		// Cancelled parses poison the parser (see releaseParser) —
		// retire it rather than pooling it.
		p.Close()
		return false, false
	}
	// The tree is self-contained; hand the parser back before querying.
	releaseParser(p)
	defer tree.Close()

	e.paintCaptures(tree, src, 0, len(src), classes)

	complete = true
	if e.inj != nil && depth < maxInjectionDepth {
		for _, site := range e.inj.collectInjections(tree.RootNode(), src) {
			if child := engineFor(site.lang); child != nil {
				_, childComplete := child.paint(src, site.ranges, classes, depth+1)
				complete = complete && childComplete
			}
		}
	}
	return true, complete
}

// paintCaptures paints the classes of e's highlight captures over
// src[lo:hi] into classes, which holds exactly that range. A capture
// that crosses the range keeps its whole length for precedence, so a
// range paints the same classes as the same bytes of a whole-document
// paint.
func (e *engine) paintCaptures(tree *tree_sitter.Tree, src []byte, lo, hi int, classes []uint16) {
	qc := acquireQueryCursor()
	defer releaseQueryCursor(qc)
	spansPtr := paintSpanPool.Get().(*[]paintSpan)
	spans := e.paintCapturesWith(qc, (*spansPtr)[:0], tree, src, lo, hi, classes)
	if cap(spans) <= maxRetainedPaintSpanCap {
		*spansPtr = spans[:0]
		paintSpanPool.Put(spansPtr)
	}
}

func (e *engine) paintCapturesWith(
	qc *tree_sitter.QueryCursor,
	spans []paintSpan,
	tree *tree_sitter.Tree,
	src []byte,
	lo, hi int,
	classes []uint16,
) []paintSpan {
	if lo > 0 || hi < len(src) {
		qc.SetByteRange(uint(lo), uint(hi))
	} else {
		qc.SetByteRange(0, uint(len(src)))
	}
	matches := qc.Matches(e.query, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		for _, c := range m.Captures {
			class := e.captureClass[c.Index]
			if class == ClassNone {
				continue
			}
			start, end := int(c.Node.StartByte()), int(c.Node.EndByte())
			if end > len(src) {
				end = len(src)
			}
			if start >= end || end <= lo || start >= hi {
				continue
			}
			spans = append(spans, paintSpan{start: start, end: end, class: class, pattern: m.PatternIndex})
		}
	}

	// Paint longest-first so narrower (more specific) captures land on
	// top of enclosing ones. Same-length overlaps resolve to the later
	// pattern in the query file — both our query sources (helix,
	// nvim-treesitter) document "last matching pattern wins", and their
	// files are ordered broad-to-specific accordingly. Remaining ties
	// keep match order, which a ranged query shares with a whole one.
	slices.SortStableFunc(spans, func(a, b paintSpan) int {
		la, lb := a.end-a.start, b.end-b.start
		if la != lb {
			return cmp.Compare(lb, la)
		}
		return cmp.Compare(a.pattern, b.pattern)
	})

	for _, s := range spans {
		for i := max(s.start, lo); i < min(s.end, hi); i++ {
			classes[i-lo] = s.class
		}
	}
	return spans
}
