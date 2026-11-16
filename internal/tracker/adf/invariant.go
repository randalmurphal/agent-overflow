package adf

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The edit invariant is Edit's safety net: whatever path an edit takes
// through the pairing code, every rich node of the original (a macro, a
// status lozenge, a panel, a column, a table's settings) that the new
// Markdown still shows must still be in the result, with the attributes
// Markdown can't show. A rich node may go only when the user removed what
// ToMarkdown wrote for it; any other loss is unexplained. Losing content
// (a macro, a lozenge, a header cell) is a Flattens warning, which
// CheckLoss refuses unless the user accepted the loss; losing dress (a
// color, an alignment, a width) is a plain warning, and the edit goes
// ahead.

// unkept lists the rich nodes of orig that out doesn't hold, and those
// it compared: subtrees the two share (every block the edit kept) hold the
// same nodes on both sides, so only the rest is inventoried. A node is
// kept by one with its id; failing that, by one with its sig whose text
// is new, when its own text is gone from out and, by gone, from the
// Markdown (an edited cell keeps its settings; two cells or items that
// trade them do not, as both texts still show). Nodes with one id are
// interchangeable; the last ones count as lost.
func unkept(orig, out *Node, gone func(rich) bool) (lost, compared []rich) {
	compared = inventorySkip(orig, treeSet(out))
	outRs := inventorySkip(out, treeSet(orig))
	have := map[string]int{}
	shown := map[[2]string]bool{} // kind and rep, on either side
	for _, r := range outRs {
		have[r.id()]++
		shown[[2]string{"out", r.kind + "\x00" + r.rep}] = true
	}
	for _, r := range compared {
		shown[[2]string{"orig", r.kind + "\x00" + r.rep}] = true
	}
	var left []rich
	for _, r := range compared {
		if have[r.id()] > 0 {
			have[r.id()]--
			continue
		}
		left = append(left, r)
	}
	if len(left) == 0 {
		return nil, compared
	}
	renamed := map[string][]string{} // sig -> the new texts of nodes with it
	for _, r := range outRs {
		if have[r.id()] > 0 {
			have[r.id()]--
			if !shown[[2]string{"orig", r.kind + "\x00" + r.rep}] {
				renamed[r.sig] = append(renamed[r.sig], r.rep)
			}
		}
	}
	took := map[string][]string{} // sig -> the renamed texts taken
	for _, r := range left {
		// Its new text holds the old (words added around it), or the old
		// text is gone from md: not two nodes that traded what they hold.
		// A block's data goes with the whole block (see blockGone), and a
		// merged block holding the text of each of its sources keeps the
		// data they agreed on for all of them.
		if r.block != "" && slices.ContainsFunc(took[r.sig], func(rep string) bool {
			return strings.Contains(rep, r.rep)
		}) {
			continue
		}
		k := -1
		if !shown[[2]string{"out", r.kind + "\x00" + r.rep}] {
			k = slices.IndexFunc(renamed[r.sig], func(rep string) bool {
				// A word's mark stays on that word alone (see wordMarksFor).
				return !r.word && strings.Contains(rep, r.rep)
			})
			if k < 0 && len(renamed[r.sig]) > 0 && (r.block != "" || gone(r)) {
				k = 0
			}
		}
		if k < 0 {
			lost = append(lost, r)
			continue
		}
		took[r.sig] = append(took[r.sig], renamed[r.sig][k])
		renamed[r.sig] = slices.Delete(renamed[r.sig], k, k+1)
	}
	return lost, compared
}

// goneFrom reports whether an entry's representation shows fewer times in
// md than in before, the original's Markdown: the user removed it.
func goneFrom(before, md string) func(rich) bool {
	plainBefore, plainMD := unescapeMD(before), unescapeMD(md)
	return func(r rich) bool {
		if r.plain {
			return countRep(plainMD, r.rep) < countRep(plainBefore, r.rep)
		}
		return countRep(md, r.rep) < countRep(before, r.rep)
	}
}

// checkKept warns, as Flattens, about each rich node of orig missing
// from out whose representation md still shows as often as before: the
// user didn't remove it, so the edit would lose it unasked. Nodes in skip
// already have a warning of their own.
func checkKept(orig, out *Node, md string, skip map[*Node]bool) []Warning {
	before := ToMarkdown(orig)
	plainBefore, plainMD := unescapeMD(before), unescapeMD(md)
	lostList, all := unkept(orig, out, goneFrom(before, md))
	lost := map[string]int{}
	for _, r := range lostList {
		lost[r.id()]++
	}
	if len(lost) == 0 {
		return nil
	}
	budget := map[[2]string]int{}
	dropped := func(r rich) bool {
		if r.pinned {
			return false
		}
		if r.block != "" {
			return blockGone(r.block, plainBefore, plainMD)
		}
		k := [2]string{r.kind, r.rep}
		b, ok := budget[k]
		if !ok {
			if r.plain {
				b = max(0, countRep(plainBefore, r.rep)-countRep(plainMD, r.rep))
			} else {
				b = max(0, countRep(before, r.rep)-countRep(md, r.rep))
			}
		}
		if !r.outer {
			budget[k] = max(0, b-1)
		} else {
			budget[k] = b
		}
		return b > 0
	}
	// Which of several like nodes was lost is open: those with a warning
	// of their own go first, then those whose representation was removed.
	for _, r := range all {
		if skip[r.n] && lost[r.id()] > 0 {
			lost[r.id()]--
		}
	}
	var unexplained []rich
	for _, r := range all {
		if skip[r.n] || lost[r.id()] == 0 {
			continue
		}
		if dropped(r) {
			lost[r.id()]--
		} else {
			unexplained = append(unexplained, r)
		}
	}
	var ws []Warning
	seen := map[Warning]bool{}
	// A word mark lost from several words of a line is one warning that
	// names them all.
	type wordsKey struct {
		line int
		kind string
	}
	words := map[wordsKey]*[]string{}
	var order []wordsKey
	for _, r := range unexplained {
		if lost[r.id()] == 0 {
			continue
		}
		lost[r.id()]--
		line := lineOf(md, r.rep)
		if r.word && r.cosmetic {
			k := wordsKey{line, r.kind}
			if words[k] == nil {
				words[k] = &[]string{}
				order = append(order, k)
			}
			if q := strconv.Quote(r.rep); !slices.Contains(*words[k], q) {
				*words[k] = append(*words[k], q)
			}
			continue
		}
		w := Warning{Line: line, Loses: richLoss(r),
			Msg: r.label + " is still in the Markdown, but the edit would lose it"}
		switch {
		case r.pinned:
			w.Msg = "dropped " + r.label + ": the edit changed the words they comment on"
		case r.cosmetic:
			w.Msg = "dropped " + r.label + ", which Markdown can't show"
		}
		if !seen[w] {
			seen[w] = true
			ws = append(ws, w)
		}
	}
	for _, k := range order {
		_, phrase, _ := strings.Cut(k.kind, "|")
		msg := "dropped the " + phrase + " of " + strings.Join(*words[k], ", ")
		if phrase == markPhrases["annotation"] {
			msg += ": the edit changed the words they comment on"
		} else {
			msg += ", which Markdown can't show"
		}
		ws = append(ws, Warning{Line: k.line, Msg: msg})
	}
	return ws
}

// countRep counts s in md as a whole: where s starts (or ends) with a
// letter or digit, the occurrence must not run on into another, so a
// cell "Property" edited to "Propertyq" no longer counts. A rep of more
// than one line matches with any indent or quote markers after each line
// break, as md writes it inside a list item, a quote or a panel.
func countRep(md, s string) int {
	if s == "" {
		return 0
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	find := func(from int) (at, end int) {
		k := strings.Index(md[from:], s)
		if k < 0 {
			return -1, -1
		}
		return from + k, from + k + len(s)
	}
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = regexp.QuoteMeta(strings.TrimLeft(l, " \t"))
		}
		re := regexp.MustCompile(strings.Join(lines, `\n[ \t>]*`))
		find = func(from int) (at, end int) {
			loc := re.FindStringIndex(md[from:])
			if loc == nil {
				return -1, -1
			}
			return from + loc[0], from + loc[1]
		}
	}
	n := 0
	for i := 0; ; {
		at, end := find(i)
		if at < 0 {
			return n
		}
		before, _ := utf8.DecodeLastRuneInString(md[:at])
		after, _ := utf8.DecodeRuneInString(md[end:])
		runsOn := isAlnum(first) && isAlnum(before) || isAlnum(last) && isAlnum(after)
		if !runsOn {
			n++
		}
		i = max(end, at+1)
	}
}

// blockGone reports whether the user deleted block, a block's Markdown
// read without escapes: every one of its lines that md showed is gone
// from md, and at least half its words show fewer times than before. Rewording a sentence
// keeps the block; deleting it, or replacing it outright, does not.
func blockGone(block, before, md string) bool {
	for l := range strings.SplitSeq(block, "\n") {
		l = strings.TrimSpace(l)
		if was := countRep(before, l); l != "" && was > 0 && countRep(md, l) >= was {
			return false
		}
	}
	words := map[string]bool{}
	for _, w := range blockWordRe.FindAllString(block, -1) {
		words[w] = true
	}
	gone := 0
	for w := range words {
		if countRep(md, w) < countRep(before, w) {
			gone++
		}
	}
	return gone*2 >= len(words)
}

var blockWordRe = regexp.MustCompile(`[\p{L}\p{N}]{3,}`)

// unescapeMD is md without the backslash before escaped punctuation, so
// text as typed counts where it shows.
func unescapeMD(md string) string {
	if !strings.Contains(md, "\\") {
		return md
	}
	var b strings.Builder
	for i := 0; i < len(md); i++ {
		if md[i] == '\\' && i+1 < len(md) && isASCIIPunct(md[i+1]) {
			continue
		}
		b.WriteByte(md[i])
	}
	return b.String()
}

// lineOf is the 1-based line of the first s in md, or 0.
func lineOf(md, s string) int {
	i := strings.Index(md, s)
	if i < 0 {
		return 0
	}
	return strings.Count(md[:i], "\n") + 1
}
