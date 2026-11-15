package store

import (
	"encoding/json"
	"regexp"
	"strings"

	"agent-overflow/internal/stringsx"
)

// Thread find's candidate filter.
//
// A pane counts and paints a row's matches in its find text, the text the
// row shows, by the find rule (frontend utils/findMatch.ts). That rule lives
// in the frontend; this side only answers which rows may hold a match. A
// row's candidate text (items.find_text, import_history_items.find_text) is
// a loose form of what the row can show, stored when the row is written, and
// a row is a candidate when its candidate text holds the loose form of the
// query. A candidate may hold no match; a row that holds one is always a
// candidate.
//
// The loose form (findLoose) is the text folded by the find rule with every
// ASCII byte that is not a letter or digit removed: markup punctuation,
// white space and line breaks. A find query is one line, and a match is a
// run of rendered text the query spells; removing the same bytes from both
// keeps every match a substring. What rendering adds or removes beyond such
// bytes is covered by variants, joined by "\n", which no loose query holds:
//
//   - every row: its summary, and its summary with the markup that renders
//     no letters erased outside code (findErased): link destinations and
//     reference labels, images, and HTML tags and comments (a character
//     reference shows as written); and the text of each table cell a
//     row-span marker merges into the cell above;
//   - tool rows: also the tool name and every string in meta and in their
//     payload's meta, where a card's command and arguments come from;
//   - reasoning rows: the full streamed content, which an expanded row
//     shows and the summary keeps only the tail of.
//
// A match the frontend counts spans no content find skips: the highlighter
// and the projections end a run at each skipped island (math, diagrams,
// images, a blocked link's tag). The variants cover each construct as shown
// or as erased, not a line that mixes both: a query spanning literal markup
// that looks like a link or tag and, on the same line, markup that renders
// erased is not a candidate. findCandidateVectors.json holds the frontend's
// find text to this filter.

// findSeparator joins candidate variants; no loose text holds it.
const findSeparator = "\n"

// findLoose is text folded for find with its ASCII non-alphanumerics
// removed. It keeps every non-ASCII code point, so it needs no Unicode
// class table beyond the fold's.
func findLoose(text string) string {
	folded := stringsx.FoldForFind(text)
	keep := 0
	for i := 0; i < len(folded); i++ {
		if c := folded[i]; c >= 0x80 || isASCIIAlnum(c) {
			keep++
		}
	}
	if keep == len(folded) {
		return folded
	}
	b := make([]byte, 0, keep)
	for i := 0; i < len(folded); i++ {
		if c := folded[i]; c >= 0x80 || isASCIIAlnum(c) {
			b = append(b, c)
		}
	}
	return string(b)
}

func isASCIIAlnum(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// findReasoningKind reports a kind whose row shows its full content once
// expanded, of which the summary holds only the tail.
func findReasoningKind(kind string) bool {
	return kind == "thinking" || kind == "compaction_reasoning"
}

func findToolKind(kind string) bool {
	return kind == "tool_call" || kind == "tool_completion"
}

// findPayloadKind reports a kind whose candidate text reads its payload.
func findPayloadKind(kind string) bool {
	return findReasoningKind(kind) || findToolKind(kind)
}

// findCandidateText is a row's candidate text. payload is the part of the
// row's payload it shows: a reasoning row's full streamed text, a tool
// row's payload meta, or nil when the row has no payload.
func findCandidateText(kind, summary, meta, toolName string, payload *string) string {
	if findReasoningKind(kind) {
		if payload != nil {
			// The content comes last, so a chunk appended to it extends it
			// (trg_payload_chunks_find_text). The summary is its tail but
			// for a summary written ahead of the content's last chunk.
			return joinFindVariants(findLoose(summary), findLoose(*payload))
		}
		return findLoose(summary)
	}
	variants := []string{findLoose(summary)}
	erased := findErased(summary)
	if erased != summary {
		variants = append(variants, findLoose(erased))
	}
	for _, merged := range findTableMerges(erased) {
		variants = append(variants, findLoose(merged))
	}
	if findToolKind(kind) {
		variants = append(variants, findLoose(toolName))
		variants = appendMetaStrings(variants, meta)
		if payload != nil {
			variants = appendMetaStrings(variants, *payload)
		}
	}
	return joinFindVariants(variants...)
}

// joinFindVariants joins the nonempty variants no other variant holds, in
// order; of equal variants the first stays. The last variant always stays
// last when it is kept.
func joinFindVariants(variants ...string) string {
	kept := make([]string, 0, len(variants))
	for i, v := range variants {
		if v == "" {
			continue
		}
		covered := false
		for j, other := range variants {
			if j != i && (len(other) > len(v) || (len(other) == len(v) && j < i)) && strings.Contains(other, v) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, v)
		}
	}
	return strings.Join(kept, findSeparator)
}

// appendMetaStrings adds the loose form of every string in meta. Meta that
// is not JSON is taken as text.
func appendMetaStrings(variants []string, meta string) []string {
	if strings.TrimSpace(meta) == "" || meta == "{}" {
		return variants
	}
	var value any
	if err := json.Unmarshal([]byte(meta), &value); err != nil {
		return append(variants, findLoose(meta))
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if loose := findLoose(v); loose != "" {
				variants = append(variants, loose)
			}
		case []any:
			for _, each := range v {
				walk(each)
			}
		case map[string]any:
			for _, each := range v {
				walk(each)
			}
		}
	}
	walk(value)
	return variants
}

var (
	findFenceOpen   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	findCodeSpan    = regexp.MustCompile("`+")
	findHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	findImage       = regexp.MustCompile(`!\[[^\]\n]*\](?:\([^()\n]*(?:\([^()\n]*\)[^()\n]*)*\)|\[[^\]\n]*\])`)
	findLinkTarget  = regexp.MustCompile(`\](?:\([^()\n]*(?:\([^()\n]*\)[^()\n]*)*\)|\[[^\]\n]*\])`)
	findHTMLTag     = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>]*)?/?>`)
)

// findErased is text with the markup that renders no letters erased
// outside fenced code blocks and code spans, which show their text as it
// is. It reads markup lexically and liberally: a construct the renderer
// shows as written stays in the summary variant.
func findErased(text string) string {
	if !strings.ContainsAny(text, "<[") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	var prose strings.Builder
	flush := func() {
		if prose.Len() > 0 {
			out.WriteString(findErasedProse(prose.String()))
			prose.Reset()
		}
	}
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		if fence != "" {
			out.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if m := findFenceOpen.FindStringSubmatch(line); m != nil {
			flush()
			fence = m[1]
			out.WriteString(line)
			continue
		}
		prose.WriteString(line)
	}
	flush()
	return out.String()
}

// findErasedProse erases markup in text outside code spans: a run of n
// backticks opens a span the next run of exactly n closes.
func findErasedProse(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	runs := findCodeSpan.FindAllStringIndex(text, -1)
	at := 0
	for i := 0; i < len(runs); i++ {
		open := runs[i]
		width := open[1] - open[0]
		closing := -1
		for j := i + 1; j < len(runs); j++ {
			if runs[j][1]-runs[j][0] == width {
				closing = j
				break
			}
		}
		if closing < 0 {
			continue
		}
		out.WriteString(eraseFindMarkup(text[at:open[0]]))
		out.WriteString(text[open[0]:runs[closing][1]])
		at = runs[closing][1]
		i = closing
	}
	out.WriteString(eraseFindMarkup(text[at:]))
	return out.String()
}

func eraseFindMarkup(text string) string {
	if !strings.ContainsAny(text, "<[") {
		return text
	}
	text = findHTMLComment.ReplaceAllString(text, "")
	text = findImage.ReplaceAllString(text, "")
	text = findLinkTarget.ReplaceAllString(text, "]")
	return findHTMLTag.ReplaceAllString(text, "")
}

var findSuperscriptCell = regexp.MustCompile(`\^[^^\n\r]+\^$`)

// findTableMerges is the text of each table cell a row-span marker merges:
// a cell ending in "^" joins its text, after a space, to the cell above it
// (frontend markdown/parser/extensions/table.ts). A row whose cell count
// differs from the row above, as a colspan makes it, is joined to every
// cell above, so a merge is never missed.
func findTableMerges(text string) []string {
	if !strings.Contains(text, "^") || !strings.Contains(text, "|") {
		return nil
	}
	var merged []string
	var above []string
	for _, line := range strings.Split(text, "\n") {
		cells := findTableCells(line)
		if cells == nil {
			above = nil
			continue
		}
		row := make([]string, len(cells))
		for i, cell := range cells {
			row[i] = cell
			if above == nil || !strings.HasSuffix(cell, "^") || findSuperscriptCell.MatchString(cell) {
				continue
			}
			own := strings.TrimSpace(strings.TrimRight(cell, "^"))
			for j, target := range above {
				if len(above) == len(cells) && j != i {
					continue
				}
				joined := target
				if own != "" && own != target && !strings.Contains(target, own) {
					joined = strings.TrimSpace(target + " " + own)
				}
				merged = append(merged, joined)
				if j == i || (len(above) != len(cells) && j == 0) {
					row[i] = joined
				}
			}
		}
		above = row
	}
	return merged
}

// findTableCells is a table line's trimmed cells, or nil for a line that
// is no table row.
func findTableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return nil
	}
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i, cell := range cells {
		cells[i] = strings.TrimSpace(cell)
	}
	return cells
}
