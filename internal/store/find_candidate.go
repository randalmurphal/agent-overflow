package store

import (
	"encoding/json"
	"regexp"
	"strings"

	"agent-overflow/internal/agentmeta"
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
// query, or is findAnyCandidate. A candidate may hold no match; a row that
// holds one is always a candidate.
//
// The loose form (findLoose) is the text folded by the find rule with every
// ASCII byte that is not a letter or digit removed: markup punctuation,
// white space and line breaks. A find query is one line (SearchThreadItems
// refuses one that is not), so a match is a run of rendered text within one
// line, which the query spells; removing the same bytes from both keeps
// every match a substring. Rendering that drops whole lines, as the bubble
// drops a typed `!command`, needs nothing: every line it keeps lies in the
// text. What rendering adds or removes within a line is covered by
// variants, joined by "\n", which no loose query holds:
//
//   - every row: its summary;
//   - assistant rows: the summary as its Markdown shows (findShownMarkdown),
//     and the text of each table cell a row-span marker merges into the cell
//     above;
//   - user rows: the summary without its attachment image blocks, the
//     command of each shell output in meta, which its command block shows
//     whether or not the typed text still holds it, and its first line with
//     white space collapsed, which a provider's agent message shows; an
//     agent's message (agentmeta) also its header line and each joined
//     member's, and the person's message joined after a report also the
//     report's data decoded;
//   - tool rows: also the tool name and every string in meta and in their
//     payload's meta, where a card's command and arguments come from;
//   - reasoning rows: the full streamed content, which an expanded row
//     shows and the summary keeps only the tail of.
//
// Why this holds every Markdown match: a run of an assistant row is its
// block's source less the spans the renderer erases (link destinations,
// images, HTML tags and comments, reference labels), cut where find skips,
// and moved only by a table merge. findShownMarkdown erases exactly those
// spans when it reads the text as the frontend's lexer does, so each run lies
// in the shown variant, or in the summary for code shown as written; when it
// cannot, the row's candidate text is findAnyCandidate, which every query
// matches. findCandidateVectors.json holds the frontend's find text to this
// filter.

// findSeparator joins candidate variants; no loose text holds it.
const findSeparator = "\n"

// findLoose is text folded for find with its ASCII non-alphanumerics
// removed. It keeps every non-ASCII code point, so it needs no Unicode
// class table beyond the fold's.
func findLoose(text string) string {
	// Single-pass ASCII fast path: avoid an intermediate FoldForFind copy
	// and second pass when the input is pure ASCII.
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 0x80 {
			return findLooseUnicode(text)
		}
		if ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') {
			continue
		}
		b := make([]byte, 0, len(text))
		b = append(b, text[:i]...)
		for ; i < len(text); i++ {
			c = text[i]
			if c >= 0x80 {
				return findLooseUnicode(text)
			}
			if ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') {
				b = append(b, c)
			} else if 'A' <= c && c <= 'Z' {
				b = append(b, c+('a'-'A'))
			}
		}
		return string(b)
	}
	return text
}

func findLooseUnicode(text string) string {
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

// findAnyCandidate is the candidate text of a row the filter cannot narrow,
// which every query matches (findMatchSQL). Joined variants never form it:
// each is nonempty, and the separator only joins them.
const findAnyCandidate = findSeparator

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
	switch kind {
	case "assistant_text":
		shown, sure := findShownMarkdown(summary)
		if !sure {
			return findAnyCandidate
		}
		if shown != summary {
			variants = append(variants, findLoose(shown))
		}
		for _, merged := range findTableMerges(shown) {
			variants = append(variants, findLoose(merged))
		}
	case "user_text":
		// The bubble shows its text as typed, less the image blocks
		// (frontend utils/userMessageMeta.ts stripAttachmentImages).
		stripped := findUserImageBlock.ReplaceAllString(summary, "")
		if stripped != summary {
			variants = append(variants, findLoose(stripped))
		}
		for _, command := range findUserShellCommands(meta) {
			variants = append(variants, findLoose(command))
		}
		// A provider's agent message (frontend utils/agentMessage.ts
		// fromProvider), and an agent's whose facts a client cannot read,
		// show the first line, white space collapsed.
		variants = append(variants, findLoose(agentmeta.FirstLine(summary)))
		attribution := findAttribution(meta)
		switch {
		case agentmeta.IsAgentMessage(attribution):
			// An agent's row shows its header line and each joined
			// member's, which its meta says (agentmeta.Headlines).
			for _, line := range agentmeta.Headlines(summary, attribution) {
				variants = append(variants, findLoose(line))
			}
		case attribution.Origin == agentmeta.OriginAgentWake:
			// The person's message joined after a report shows the
			// report's quoted data decoded (userMessageVisibleText).
			variants = append(variants, findLoose(agentmeta.ReadableWakeText(stripped)))
		}
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

// findAttribution is the attribution a user row's meta records; none for
// meta that does not decode, which no reader attributes either.
func findAttribution(meta string) agentmeta.Meta {
	var attribution agentmeta.Meta
	if strings.TrimSpace(meta) == "" || json.Unmarshal([]byte(meta), &attribution) != nil {
		return agentmeta.Meta{}
	}
	return attribution
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

// findUserShellCommands is the command of each shell output a user row's
// meta lists (usermessage.ShellMeta), the text its command block shows. Meta
// that does not decode lists none: the frontend shows no block for it
// (userMessageMeta.ts parseUserMessageMeta).
func findUserShellCommands(meta string) []string {
	if !strings.Contains(meta, `"shell"`) {
		return nil
	}
	var decoded struct {
		Attachments []struct {
			Shell *struct {
				Command string `json:"command"`
			} `json:"shell"`
		} `json:"attachments"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return nil
	}
	var commands []string
	for _, attachment := range decoded.Attachments {
		if attachment.Shell != nil {
			commands = append(commands, attachment.Shell.Command)
		}
	}
	return commands
}

var (
	findUserImageBlock  = regexp.MustCompile(`\n\n!\[[^\]]*\]\(attachment://[^\s)]+\)`)
	findSuperscriptCell = regexp.MustCompile(`\^[^^\n\r]+\^$`)
)

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
