package highlight

import (
	"sort"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// Language injection: a host grammar (markdown, html, svelte) marks
// regions as another language via an injections query; those regions
// re-parse with the injected grammar restricted to their byte ranges
// (Parser.SetIncludedRanges keeps coordinates in host-document space)
// and paint over the host's classes. Depth is capped by
// maxInjectionDepth.
//
// Supported query features — exactly what the vendored queries use:
// @injection.content / @injection.language captures, and the
// properties injection.language, injection.combined,
// injection.include-children, injection.include-unnamed-children.
// Text predicates (#eq?, #any-of?, #match?) apply through the standard
// query matcher.

// childInclusion is the content-range policy for an injection node.
type childInclusion int

const (
	// excludeChildren (default): only the node's direct text; every
	// child node's range is subtracted.
	excludeChildren childInclusion = iota
	// includeUnnamedChildren: subtract only named children (markdown's
	// block_continuation quote markers stay out, raw tokens stay in).
	includeUnnamedChildren
	// includeChildren: the node's whole range.
	includeChildren
)

// injectionQuery is a compiled injections query plus its per-pattern
// metadata, precomputed at engine build.
type injectionQuery struct {
	query       *tree_sitter.Query
	contentIdx  uint // capture index of @injection.content
	languageIdx int  // capture index of @injection.language, -1 when absent
	patterns    []injectionPattern
}

type injectionPattern struct {
	language  string // static language from #set! injection.language, "" = read from capture
	combined  bool
	inclusion childInclusion
}

// compileInjections builds the injection metadata for a grammar, or
// nil when it has no injections query. An injections query without an
// @injection.content capture is a vendoring defect.
func compileInjections(lang *tree_sitter.Language, source string) (*injectionQuery, error) {
	if source == "" {
		return nil, nil
	}
	query, err := tree_sitter.NewQuery(lang, source)
	if err != nil {
		return nil, err
	}
	contentIdx, ok := query.CaptureIndexForName("injection.content")
	if !ok {
		query.Close()
		return nil, nil
	}
	languageIdx := -1
	if idx, ok := query.CaptureIndexForName("injection.language"); ok {
		languageIdx = int(idx)
	}
	patterns := make([]injectionPattern, query.PatternCount())
	for i := range patterns {
		for _, prop := range query.PropertySettings(uint(i)) {
			switch prop.Key {
			case "injection.language":
				if prop.Value != nil {
					patterns[i].language = *prop.Value
				}
			case "injection.combined":
				patterns[i].combined = true
			case "injection.include-children":
				patterns[i].inclusion = includeChildren
			case "injection.include-unnamed-children":
				patterns[i].inclusion = includeUnnamedChildren
			}
		}
	}
	return &injectionQuery{
		query:       query,
		contentIdx:  contentIdx,
		languageIdx: languageIdx,
		patterns:    patterns,
	}, nil
}

// injectionSite is one resolved injection: a target language and the
// host-document ranges to parse as that language.
type injectionSite struct {
	lang   Lang
	ranges []tree_sitter.Range
}

// collectInjections resolves an injections query over a parsed host
// tree. When one content node matches several patterns, the later
// pattern wins (queries order broad-to-specific, same convention as
// highlight captures). Combined patterns merge all their content
// nodes into a single site so fragments parse as one document.
func (iq *injectionQuery) collectInjections(root *tree_sitter.Node, src []byte) []injectionSite {
	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()
	matches := qc.Matches(iq.query, root, src)

	type entry struct {
		order   int
		pattern int
		lang    Lang
		ranges  []tree_sitter.Range
	}
	byNode := map[uint64]entry{} // keyed by content byte range; later matches replace earlier
	order := 0

	for m := matches.Next(); m != nil; m = matches.Next() {
		pattern := iq.patterns[m.PatternIndex]
		language := pattern.language
		var content []*tree_sitter.Node
		for i := range m.Captures {
			c := &m.Captures[i]
			if c.Index == uint32(iq.contentIdx) {
				content = append(content, &c.Node)
			}
			if iq.languageIdx >= 0 && c.Index == uint32(iq.languageIdx) && language == "" {
				language = string(src[c.Node.StartByte():c.Node.EndByte()])
			}
		}
		lang := LangFromName(language)
		if language == "" || lang == LangPlaintext || len(content) == 0 {
			continue
		}
		for _, node := range content {
			ranges := contentRanges(node, pattern.inclusion)
			if len(ranges) == 0 {
				continue
			}
			key := uint64(node.StartByte())<<32 | uint64(node.EndByte())
			byNode[key] = entry{order: order, pattern: int(m.PatternIndex), lang: lang, ranges: ranges}
			order++
		}
	}
	if len(byNode) == 0 {
		return nil
	}

	entries := make([]entry, 0, len(byNode))
	for _, e := range byNode {
		entries = append(entries, e)
	}
	// Document order: SetIncludedRanges requires sorted, non-overlapping
	// ranges, and combined sites must accumulate fragments in order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ranges[0].StartByte < entries[j].ranges[0].StartByte
	})
	// Combined patterns merge into one site per pattern.
	var sites []injectionSite
	combined := map[int]int{} // pattern index → sites index
	for _, e := range entries {
		if iq.patterns[e.pattern].combined {
			if idx, ok := combined[e.pattern]; ok {
				sites[idx].ranges = append(sites[idx].ranges, e.ranges...)
				continue
			}
			combined[e.pattern] = len(sites)
		}
		sites = append(sites, injectionSite{lang: e.lang, ranges: e.ranges})
	}
	return sites
}

// contentRanges converts a content node to its included byte ranges
// under the pattern's child-inclusion policy.
func contentRanges(node *tree_sitter.Node, inclusion childInclusion) []tree_sitter.Range {
	full := node.Range()
	if inclusion == includeChildren || node.ChildCount() == 0 {
		if full.StartByte >= full.EndByte {
			return nil
		}
		return []tree_sitter.Range{full}
	}
	var ranges []tree_sitter.Range
	cursorByte, cursorPoint := full.StartByte, full.StartPoint
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if inclusion == includeUnnamedChildren && !child.IsNamed() {
			continue
		}
		cr := child.Range()
		if cr.StartByte > cursorByte {
			ranges = append(ranges, tree_sitter.Range{
				StartByte: cursorByte, EndByte: cr.StartByte,
				StartPoint: cursorPoint, EndPoint: cr.StartPoint,
			})
		}
		if cr.EndByte > cursorByte {
			cursorByte, cursorPoint = cr.EndByte, cr.EndPoint
		}
	}
	if full.EndByte > cursorByte {
		ranges = append(ranges, tree_sitter.Range{
			StartByte: cursorByte, EndByte: full.EndByte,
			StartPoint: cursorPoint, EndPoint: full.EndPoint,
		})
	}
	return ranges
}
