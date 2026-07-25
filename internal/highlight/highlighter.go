package highlight

import (
	"bytes"
	"sort"
	"strings"
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
	truncated := false
	incomplete := false
	budget := time.Now().Add(patchParseBudget)
	primer := primer{content: fileContent}

	for _, hunk := range parsed.hunks {
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
		oldLines, oldRes := highlightHunkDoc(lang, prime, suffix, hunk.oldDoc)
		newLines, newRes := highlightHunkDoc(lang, prime, suffix, hunk.newDoc)
		truncated = truncated || oldRes.Truncated || newRes.Truncated
		incomplete = incomplete || oldRes.Incomplete || newRes.Incomplete
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
			}
			out[ref.patchIndex] = padRuns(line, ref.outPad)
		}
	}
	return Result{Lines: out, Truncated: truncated, Incomplete: incomplete}
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

// highlightHunkDoc highlights one reconstructed hunk side, optionally
// spliced between preceding and following file content. Empty docs
// (the old side of an added file) short-circuit; only the doc's own
// lines are returned.
func highlightHunkDoc(lang Lang, prime, suffix string, doc []byte) ([]EncodedLine, Result) {
	if len(doc) == 0 {
		return nil, Result{}
	}
	if prime == "" && suffix == "" {
		res := Highlight(lang, doc)
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
			res := Highlight(lang, doc)
			return res.Lines, res
		}
		cut := strings.IndexByte(prime[overflow:], '\n')
		if cut < 0 {
			res := Highlight(lang, doc)
			return res.Lines, res
		}
		prime = prime[overflow+cut+1:]
	}
	size := len(prime) + 1 + len(doc)
	if suffix != "" {
		size += 1 + len(suffix)
	}
	combined := make([]byte, 0, size)
	if prime != "" {
		combined = append(combined, prime...)
		combined = append(combined, '\n')
	}
	combined = append(combined, doc...)
	if suffix != "" {
		combined = append(combined, '\n')
		combined = append(combined, suffix...)
	}
	res := Highlight(lang, combined)
	skip := 0
	if prime != "" {
		skip = countLines([]byte(prime))
	}
	if skip >= len(res.Lines) {
		return nil, res
	}
	// Drop the suffix's trailing entries: callers index doc lines only,
	// and the trimmed slice keeps that contract visible.
	docLines := countLines(doc)
	lines := res.Lines[skip:]
	if len(lines) > docLines {
		lines = lines[:docLines]
	}
	return lines, res
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
	tree := parseWithDeadline(p, src)
	if tree == nil {
		// Cancelled parses poison the parser (see releaseParser) —
		// retire it rather than pooling it.
		p.Close()
		return false, false
	}
	// The tree is self-contained; hand the parser back before querying.
	releaseParser(p)
	defer tree.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()
	matches := qc.Matches(e.query, tree.RootNode(), src)

	spans := make([]paintSpan, 0, 256)
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
			if start >= end {
				continue
			}
			spans = append(spans, paintSpan{start: start, end: end, class: class, pattern: m.PatternIndex})
		}
	}

	// Paint longest-first so narrower (more specific) captures land on
	// top of enclosing ones. Same-length overlaps resolve to the later
	// pattern in the query file — both our query sources (helix,
	// nvim-treesitter) document "last matching pattern wins", and their
	// files are ordered broad-to-specific accordingly.
	sort.Slice(spans, func(i, j int) bool {
		li, lj := spans[i].end-spans[i].start, spans[j].end-spans[j].start
		if li != lj {
			return li > lj
		}
		return spans[i].pattern < spans[j].pattern
	})

	for _, s := range spans {
		for i := s.start; i < s.end; i++ {
			classes[i] = s.class
		}
	}

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
