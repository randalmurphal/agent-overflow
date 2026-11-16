package adf

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// inlineCtx is where a run of inline content is rendered; it decides how
// hard breaks look and which characters are syntax there.
type inlineCtx int

const (
	ctxPara    inlineCtx = iota // a paragraph line start is block syntax
	ctxHeading                  // after "# ", on one line
	ctxCell                     // inside a GFM table cell, on one line
	ctxTask                     // after "- [ ] "
)

// renderInline renders inline nodes with the lightest escaping that
// still parses back to what fully escaped text parses to. Most text
// needs no backslashes at all; when a guess is wrong, the parse check
// catches it and escaping grows one category at a time.
func renderInline(ns []*Node, ctx inlineCtx) string {
	return renderItems(inlineItems(ns, ctx), ctx, true)
}

// renderItems is renderInline on built items. With skipInert, runs whose
// unescaped text Markdown can't misread (see inert) skip the parse
// check; tests turn that off to confirm the shortcut changes nothing.
func renderItems(items []item, ctx inlineCtx, skipInert bool) string {
	s, _ := renderVerified(items, ctx, skipInert)
	return s
}

// renderVerified renders items and checks the result reads back with the
// same text and marks. Where no escaping makes a mark parse (emphasis
// on punctuation between letters: a*\_*b), that mark is dropped from the
// offending text instead of writing literal delimiters; dropped reports
// it, and lossy lists it as formatting Markdown can't show.
func renderVerified(items []item, ctx inlineCtx, skipInert bool) (
	s string, dropped bool) {
	for {
		out, full, toks, checked := renderOnce(items, ctx, skipInert)
		if !checked {
			return out, dropped
		}
		d, ok := sigDiff(full, toks, ctx)
		if !ok || d < 0 {
			return out, dropped
		}
		next, fixed := dropOffending(items, ctx, d)
		if !fixed {
			return out, dropped
		}
		items, dropped = next, true
	}
}

// renderOnce is one rendering of items: the chosen string, the fully
// escaped string and the tokens it came from, and whether it was parse
// checked (inert text is not).
func renderOnce(items []item, ctx inlineCtx, skipInert bool) (
	out, full string, linked []tok, checked bool) {
	if len(items) == 0 {
		return "", "", nil, false
	}
	// Tokens depend on the level only through escLinks.
	bare := markTokens(items, 0)
	minimal := emitTokens(bare, ctx, 0)
	isInert := skipInert && inert(bare, ctx)
	if isInert && !hasDelims(bare) {
		return minimal, "", nil, false
	}
	linked = markTokens(items, escLinks)
	emit := func(l escLevel) string {
		if l&escLinks != 0 {
			return emitTokens(linked, ctx, l)
		}
		return emitTokens(bare, ctx, l)
	}
	full = emit(escAll | escFull)
	if isInert {
		// Inert text reads the same escaped or not, but whether the
		// delimiters around it flank still needs the check.
		return minimal, full, linked, true
	}
	if minimal == full {
		return minimal, full, linked, true
	}
	want, ok := parseKey(full, ctx)
	if !ok {
		return full, full, linked, false
	}
	if got, ok := parseKey(minimal, ctx); ok && got == want {
		return minimal, full, linked, true
	}
	for _, l := range escLadder {
		s := emit(l)
		if got, ok := parseKey(s, ctx); ok && got == want {
			return s, full, linked, true
		}
	}
	return full, full, linked, true
}

// hasDelims reports emphasis or strike delimiters, which may fail to
// flank; links and HTML-tag marks always parse.
func hasDelims(toks []tok) bool {
	for _, t := range toks {
		if t.kind == tkOpen && flanking(t.mark) {
			return true
		}
	}
	return false
}

// sigDiff compares what md parses to with what toks meant, as one entry
// per non-space character ("c:marks") or inline node ("{type}"). It
// returns the first differing entry, -1 when they agree, and false when
// either side can't be read.
func sigDiff(md string, toks []tok, ctx inlineCtx) (int, bool) {
	got, ok := parseSig(md, ctx)
	if !ok {
		return 0, false
	}
	want, ok := tokenSig(toks, ctx)
	if !ok {
		return 0, false
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return i, true
		}
	}
	if len(got) != len(want) {
		return min(len(got), len(want)), true
	}
	return -1, true
}

// parseSig parses md in ctx and lists its inline content (see sigDiff).
func parseSig(md string, ctx inlineCtx) ([]string, bool) {
	switch ctx {
	case ctxHeading:
		md = "# " + md
	case ctxCell:
		md = "| |\n| --- |\n| " + md + " |"
	case ctxTask:
		md = "- [ ] " + strings.ReplaceAll(md, "\n", "\n  ")
	}
	c := newConverter(context.Background(), md, nil, true)
	blocks, err := c.document()
	if err != nil {
		return nil, false
	}
	var out []string
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			switch {
			case n.Type == "text":
				out = appendRunes(out, n.Text, n.Marks, "")
			case isInline(n.Type):
				out = append(out, "{"+n.Type+"}")
			default:
				walk(n.Content)
			}
		}
	}
	walk(blocks)
	return out, true
}

// tokenSig lists the inline content toks mean (see sigDiff); raw markup
// counts as what it parses to on its own.
func tokenSig(toks []tok, ctx inlineCtx) ([]string, bool) {
	var out []string
	var stack []Mark
	for _, t := range toks {
		switch t.kind {
		case tkOpen:
			stack = append(stack, t.mark)
		case tkClose:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case tkText:
			out = appendRunes(out, t.s, stack, "")
		case tkCode:
			out = appendRunes(out, t.s, stack, "code")
		case tkBreak:
			out = append(out, "{hardBreak}")
		case tkRaw:
			sig, ok := parseSig(t.s, ctx)
			if !ok {
				return nil, false
			}
			out = append(out, sig...)
		}
	}
	return out, true
}

func appendRunes(out []string, s string, marks []Mark, extra string) []string {
	types := make([]string, 0, len(marks)+1)
	for _, m := range marks {
		types = append(types, m.Type)
	}
	if extra != "" {
		types = append(types, extra)
	}
	slices.Sort(types)
	sig := strings.Join(types, "+")
	for _, r := range s {
		if !unicode.IsSpace(r) {
			out = append(out, string(r)+":"+sig)
		}
	}
	return out
}

// dropOffending removes one mark near signature entry d from the items:
// from the item holding entry d or a neighbor, the first removal after
// which the rendering reads back correctly at least past d.
func dropOffending(items []item, ctx inlineCtx, d int) ([]item, bool) {
	at, n := -1, 0
	for i, it := range items {
		switch it.kind {
		case tkText, tkCode:
			n += len(appendRunes(nil, it.s, nil, ""))
		case tkBreak:
			n++
		case tkRaw:
			sig, _ := parseSig(it.s, ctx)
			n += len(sig)
		}
		if n > d {
			at = i
			break
		}
	}
	if at < 0 {
		at = len(items) - 1
	}
	// A candidate is judged on its fully escaped rendering alone: one
	// parse, not the escape-level search renderOnce makes for output.
	progress := func(next []item) bool {
		if len(next) == 0 {
			return false
		}
		toks := markTokens(next, escLinks)
		nd, ok := sigDiff(emitTokens(toks, ctx, escAll|escFull), toks, ctx)
		return ok && (nd < 0 || nd > d)
	}
	near := []int{at, at - 1, at + 1}
	for _, k := range near {
		if k < 0 || k >= len(items) {
			continue
		}
		for m := range items[k].marks {
			next := slices.Clone(items)
			next[k].marks = slices.Delete(slices.Clone(next[k].marks), m, m+1)
			if progress(next) {
				return next, true
			}
		}
	}
	// No single mark: one item's text can't carry any of its marks here.
	for _, k := range near {
		if k >= 0 && k < len(items) && len(items[k].marks) > 1 {
			next := slices.Clone(items)
			next[k].marks = nil
			if progress(next) {
				return next, true
			}
		}
	}
	return nil, false
}

// parseKey parses rendered inline Markdown in its context and returns a
// canonical key for the result.
func parseKey(md string, ctx inlineCtx) (string, bool) {
	switch ctx {
	case ctxHeading:
		md = "# " + md
	case ctxCell:
		md = "| |\n| --- |\n| " + md + " |"
	case ctxTask:
		md = "- [ ] " + strings.ReplaceAll(md, "\n", "\n  ")
	}
	c := newConverter(context.Background(), md, nil, true)
	blocks, err := c.document()
	if err != nil {
		return "", false
	}
	return key(Doc(blocks...)), true
}

type tokKind uint8

const (
	tkText  tokKind = iota // text, escaped as needed
	tkRaw                  // markup written as is
	tkCode                 // inline code
	tkBreak                // hard break
	tkOpen                 // mark start
	tkClose                // mark end
)

type item struct {
	kind  tokKind
	s     string
	marks []Mark
}

// renderable lists the marks Markdown can show; the rest are dropped.
var renderable = map[string]bool{
	"link": true, "strong": true, "em": true, "strike": true,
	"underline": true, "subsup": true,
}

func inlineItems(ns []*Node, ctx inlineCtx) []item {
	var out []item
	raw := func(s string) { out = append(out, item{kind: tkRaw, s: s}) }
	txt := func(s string) { out = append(out, item{kind: tkText, s: s}) }
	for _, n := range ns {
		switch n.Type {
		case "text":
			var ms []Mark
			for _, m := range n.Marks {
				if renderable[m.Type] {
					ms = append(ms, m)
				}
			}
			kind := tkText
			if hasMark(n, "code") {
				kind = tkCode
			}
			out = append(out, item{kind: kind, s: newlinesToSpaces(n.Text),
				marks: ms})
		case "hardBreak":
			out = append(out, item{kind: tkBreak})
		case "mention":
			if strings.TrimSpace(n.attrStr("id")) == "" {
				// @[accountid:] would not parse; keep what it showed.
				txt(cmp.Or(n.attrStr("text"), "@"))
			} else {
				raw(mentionMarkdown(n, ctx))
			}
		case "emoji":
			if t := emojiText(n); t != "" {
				txt(t)
			}
		case "status":
			// As stored: a lozenge typed "[In review]" reads back as typed.
			txt("[" + n.attrStr("text") + "]")
		case "date":
			txt(formatDate(n.attrStr("timestamp")))
		case "placeholder":
		case "inlineCard":
			raw(cardMarkdown(n, ctx))
		case "mediaInline":
			raw(cellPipes(mediaLink(n), ctx))
		case "inlineExtension":
			raw(macroComment(n))
		default:
			if len(n.Content) > 0 {
				out = append(out, inlineItems(n.Content, ctx)...)
			} else {
				raw("<!-- adf: " + commentSafe(n.Type) + " -->")
			}
		}
	}
	return trimItems(mergeItems(out))
}

var (
	newlineSpacer  = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
	bracketEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	titleEscaper   = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
)

// newlinesToSpaces replaces line breaks inside a text node, which
// Markdown can't keep, with spaces.
func newlinesToSpaces(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return newlineSpacer.Replace(s)
}

func cellPipes(s string, ctx inlineCtx) string {
	if ctx == ctxCell {
		return strings.ReplaceAll(s, "|", `\|`)
	}
	return s
}

func mentionMarkdown(n *Node, ctx inlineCtx) string {
	name := mentionName(n)
	if name == "" {
		name = "accountid:" + n.attrStr("id")
	}
	return cellPipes("@["+bracketEscaper.Replace(name)+"]", ctx)
}

// mentionName is the name @[...] is written with: the display text
// without its @, on one line and trimmed as the write path reads it.
func mentionName(n *Node) string {
	name := strings.TrimPrefix(n.attrStr("text"), "@")
	return strings.Join(strings.Fields(name), " ")
}

var autolinkRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*$`)

func cardMarkdown(n *Node, ctx inlineCtx) string {
	u := n.attrStr("url")
	if u == "" {
		return "<!-- adf: inlineCard -->"
	}
	if autolinkRe.MatchString(u) && (ctx != ctxCell ||
		!strings.Contains(u, "|")) {
		return "<" + u + ">"
	}
	label := bracketEscaper.Replace(u)
	return cellPipes("["+label+"]("+linkDest(u, ctx == ctxCell)+")", ctx)
}

func mergeItems(in []item) []item {
	var out []item
	for _, it := range in {
		k := len(out) - 1
		if k >= 0 && it.kind == out[k].kind &&
			(it.kind == tkText || it.kind == tkCode) &&
			marksEqual(it.marks, out[k].marks) {
			out[k].s += it.s
			continue
		}
		out = append(out, it)
	}
	return out
}

// trimItems drops whitespace at line edges, which Markdown can't keep.
func trimItems(in []item) []item {
	out := append([]item(nil), in...)
	edge := func(i int) bool {
		return i < 0 || i >= len(out) || out[i].kind == tkBreak
	}
	for i := range out {
		if out[i].kind != tkText {
			continue
		}
		if edge(i - 1) {
			out[i].s = strings.TrimLeft(out[i].s, " \t")
		}
		if edge(i + 1) {
			out[i].s = strings.TrimRight(out[i].s, " \t")
		}
	}
	keep := out[:0]
	for _, it := range out {
		if it.kind == tkText && it.s == "" {
			continue
		}
		keep = append(keep, it)
	}
	if len(keep) != len(in) {
		return trimItems(mergeItems(keep))
	}
	return keep
}

type tok struct {
	kind   tokKind
	s      string
	mark   Mark
	pair   int
	inLink bool
}

func findMark(ms []Mark, m Mark) bool {
	for _, x := range ms {
		if markEqual(x, m) {
			return true
		}
	}
	return false
}

var openRank = map[string]int{
	"link": 0, "strong": 1, "em": 2, "strike": 3, "underline": 4,
	"subsup": 5,
}

// tagMark reports marks written as HTML tags (<u>, <sub>, <sup>).
func tagMark(m Mark) bool { return m.Type == "underline" || m.Type == "subsup" }

// tagsDiffer reports whether item marks ms hold a different set of tag
// marks than stack.
func tagsDiffer(ms, stack []Mark) bool {
	n := 0
	for _, m := range stack {
		if tagMark(m) {
			if !findMark(ms, m) {
				return true
			}
			n++
		}
	}
	for _, m := range ms {
		if tagMark(m) {
			n--
		}
	}
	return n != 0
}

// markTokens turns items into tokens with mark delimiters. Marks that
// run longest open first, so they close last and are not reopened.
// HTML-tag marks are the exception: they always open outside emphasis,
// and emphasis closes and reopens around each tag. Inside, a tag's < or >
// would sit against an emphasis delimiter and stop it flanking:
// **a <u>b</u>**c reads back as literal asterisks, **a** <u>**b**</u>c
// does not.
func markTokens(items []item, lvl escLevel) []tok {
	items = bareLinks(items, lvl)
	var toks []tok
	var stack []Mark
	closeTo := func(k int) {
		for len(stack) > k {
			toks = append(toks, tok{kind: tkClose, mark: stack[len(stack)-1]})
			stack = stack[:len(stack)-1]
		}
	}
	for i, it := range items {
		k := 0
		for k < len(stack) && findMark(it.marks, stack[k]) {
			k++
		}
		if tagsDiffer(it.marks, stack) {
			for j, m := range stack[:k] {
				if flanking(m) {
					k = j
					break
				}
			}
		}
		closeTo(k)
		var add []Mark
		for _, m := range it.marks {
			if !findMark(stack, m) {
				add = append(add, m)
			}
		}
		slices.SortStableFunc(add, func(a, b Mark) int {
			if ta, tb := tagMark(a), tagMark(b); ta != tb {
				if ta {
					return -1
				}
				return 1
			}
			return cmp.Or(
				cmp.Compare(runLength(items, i, b), runLength(items, i, a)),
				cmp.Compare(openRank[a.Type], openRank[b.Type]))
		})
		for _, m := range add {
			toks = append(toks, tok{kind: tkOpen, mark: m})
			stack = append(stack, m)
		}
		toks = append(toks, tok{kind: it.kind, s: it.s})
	}
	closeTo(0)
	// Moving a space out of one mark can leave it at the edge of the next
	// ([strong "a", strong+em " "] becomes **a **), so repeat until the
	// edges settle.
	for range 16 {
		before := slices.Clone(toks)
		toks = dropEmptyPairs(moveSpaces(movePunct(moveSpaces(toks))))
		if slices.EqualFunc(before, toks, sameTok) {
			break
		}
	}
	return pairUp(toks)
}

func sameTok(a, b tok) bool {
	return a.kind == b.kind && a.s == b.s && markEqual(a.mark, b.mark)
}

func runLength(items []item, i int, m Mark) int {
	n := 0
	for ; i < len(items) && findMark(items[i].marks, m); i++ {
		n++
	}
	return n
}

// bareLinks writes a link whose text is its own URL as the bare URL,
// which GFM autolinks back to the same link.
func bareLinks(items []item, lvl escLevel) []item {
	if lvl&escLinks != 0 {
		return items
	}
	out := make([]item, len(items))
	for i, it := range items {
		out[i] = it
		if it.kind != tkText {
			continue
		}
		for j, m := range it.marks {
			if m.Type != "link" || len(m.Attrs) != 1 {
				continue
			}
			href, _ := m.Attrs["href"].(string)
			if !bareURL(it.s, href) {
				continue
			}
			ms := append(append([]Mark(nil), it.marks[:j]...),
				it.marks[j+1:]...)
			out[i] = item{kind: tkRaw, s: it.s, marks: ms}
			break
		}
	}
	return out
}

var (
	bareURLRe   = regexp.MustCompile(`^(https?|ftp)://[^\s<>|\\` + "`" + `]+$`)
	bareMailRe  = regexp.MustCompile(`^[A-Za-z0-9._+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
	bareWWWRe   = regexp.MustCompile(`^www\.[^\s<>|\\` + "`" + `]+$`)
	urlTailPunc = "?!.,:*_~'\";)"
)

func bareURL(text, href string) bool {
	if text == "" || strings.ContainsAny(text[len(text)-1:], urlTailPunc) {
		return false
	}
	switch {
	case text == href:
		return bareURLRe.MatchString(text)
	case "mailto:"+text == href:
		return bareMailRe.MatchString(text)
	case "http://"+text == href:
		return bareWWWRe.MatchString(text)
	}
	return false
}

func flanking(m Mark) bool {
	return m.Type == "strong" || m.Type == "em" || m.Type == "strike"
}

// moveSpaces moves whitespace at the inside edge of emphasis outside
// it: "** bold **" is not emphasis in Markdown, " **bold** " is. Any
// Unicode space counts, NBSP included, as it does for CommonMark.
func moveSpaces(toks []tok) []tok {
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tkText {
			continue
		}
		if lead := t.s[:len(t.s)-len(strings.TrimLeftFunc(t.s, unicode.IsSpace))]; lead != "" {
			j := i
			for j > 0 && toks[j-1].kind == tkOpen && flanking(toks[j-1].mark) {
				j--
			}
			if j < i {
				toks[i].s = t.s[len(lead):]
				toks = insertTok(toks, j, tok{kind: tkText, s: lead})
				i++
				t = toks[i]
			}
		}
		if trail := t.s[len(strings.TrimRightFunc(t.s, unicode.IsSpace)):]; trail != "" {
			j := i
			for j+1 < len(toks) && toks[j+1].kind == tkClose &&
				flanking(toks[j+1].mark) {
				j++
			}
			if j > i {
				toks[i].s = t.s[:len(t.s)-len(trail)]
				toks = insertTok(toks, j+1, tok{kind: tkText, s: trail})
			}
		}
	}
	out := toks[:0]
	for _, t := range toks {
		if t.kind == tkText && t.s == "" {
			continue
		}
		if k := len(out) - 1; k >= 0 && t.kind == tkText &&
			out[k].kind == tkText {
			out[k].s += t.s
			continue
		}
		out = append(out, t)
	}
	return out
}

// movePunct moves punctuation at the inside edge of emphasis outside it
// when a letter or digit sits on the outside: CommonMark can't open
// emphasis in "3**.x**", so the text would show literal asterisks.
func movePunct(toks []tok) []tok {
	isP := isPunctRune
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tkText {
			continue
		}
		rs := []rune(toks[i].s)
		j := i
		for j > 0 && toks[j-1].kind == tkOpen && flanking(toks[j-1].mark) {
			j--
		}
		if j < i && j > 0 && endsAlnum(toks[j-1]) {
			k := 0
			for k < len(rs) && isP(rs[k]) {
				k++
			}
			if k > 0 && k < len(rs) {
				toks[i].s = string(rs[k:])
				toks = insertTok(toks, j, tok{kind: tkText, s: string(rs[:k])})
				i++
				rs = rs[k:]
			}
		}
		j = i
		for j+1 < len(toks) && toks[j+1].kind == tkClose &&
			flanking(toks[j+1].mark) {
			j++
		}
		if j > i && j+1 < len(toks) && startsAlnum(toks[j+1]) {
			k := len(rs)
			for k > 0 && isP(rs[k-1]) {
				k--
			}
			if k < len(rs) && k > 0 {
				toks[i].s = string(rs[:k])
				toks = insertTok(toks, j+1, tok{kind: tkText,
					s: string(rs[k:])})
			}
		}
	}
	return mergeTextToks(toks)
}

func endsAlnum(t tok) bool {
	if t.kind != tkText && t.kind != tkRaw {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(t.s)
	return isAlnum(r)
}

func startsAlnum(t tok) bool {
	if t.kind != tkText && t.kind != tkRaw {
		return false
	}
	r, _ := utf8.DecodeRuneInString(t.s)
	return isAlnum(r)
}

func insertTok(toks []tok, at int, t tok) []tok {
	toks = append(toks, tok{})
	copy(toks[at+1:], toks[at:])
	toks[at] = t
	return toks
}

// dropEmptyPairs removes delimiters with nothing between them.
func dropEmptyPairs(toks []tok) []tok {
	for {
		changed := false
		out := toks[:0]
		for i := 0; i < len(toks); i++ {
			if i+1 < len(toks) && toks[i].kind == tkOpen &&
				toks[i+1].kind == tkClose &&
				markEqual(toks[i].mark, toks[i+1].mark) {
				i++
				changed = true
				continue
			}
			out = append(out, toks[i])
		}
		toks = out
		if !changed {
			return mergeTextToks(toks)
		}
	}
}

func mergeTextToks(toks []tok) []tok {
	out := toks[:0]
	for _, t := range toks {
		if k := len(out) - 1; k >= 0 && t.kind == tkText &&
			out[k].kind == tkText {
			out[k].s += t.s
			continue
		}
		out = append(out, t)
	}
	return out
}

// pairUp links each open to its close and marks text inside links.
func pairUp(toks []tok) []tok {
	var stack []int
	links := 0
	for i := range toks {
		switch toks[i].kind {
		case tkOpen:
			stack = append(stack, i)
			if toks[i].mark.Type == "link" {
				links++
			}
		case tkClose:
			if k := len(stack) - 1; k >= 0 {
				toks[i].pair, toks[stack[k]].pair = stack[k], i
				stack = stack[:k]
			}
			if toks[i].mark.Type == "link" {
				links--
			}
		default:
			toks[i].inLink = links > 0
		}
	}
	return toks
}

// emitTokens renders tokens built by markTokens at the same level.
func emitTokens(toks []tok, ctx inlineCtx, lvl escLevel) string {
	w := &writer{toks: toks, ctx: ctx, lvl: lvl}
	w.countRuns()
	return guardHTMLLines(w.write(), ctx)
}

var (
	tagsOnly = regexp.MustCompile(`^(\s*(</?[A-Za-z][A-Za-z0-9-]*` +
		`(\s[^>]*)?/?>|<!--.*?-->))+\s*$`)
	htmlBlockish = regexp.MustCompile(`^(<!--|<\?|<![A-Za-z]|<!\[CDATA\[)`)
)

// guardHTMLLines keeps a line that holds only tags (or starts a comment)
// from being read as an HTML block, by starting it with an encoded space,
// which the write path trims. A paragraph of only <br> tags needs no
// guard: the write path reads that HTML block as hard breaks.
func guardHTMLLines(s string, ctx inlineCtx) string {
	if ctx == ctxCell || ctx == ctxHeading || !strings.Contains(s, "<") ||
		brOnlyRe.MatchString(s) {
		return s
	}
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		first := i == 0 && ctx == ctxPara
		if (first && tagsOnly.MatchString(l)) || (i > 0 || first) &&
			htmlBlockish.MatchString(l) {
			ls[i] = "&#32;" + l
		}
	}
	return strings.Join(ls, "\n")
}

type writer struct {
	toks []tok
	ctx  inlineCtx
	lvl  escLevel
	b    strings.Builder
	runs [3]int // delimiter-capable runs of * _ ~ (delimSlot)
	tick int
}

func (w *writer) write() string {
	lastContent := -1
	firstContent := len(w.toks)
	for i, t := range w.toks {
		if t.kind != tkBreak && t.kind != tkOpen && t.kind != tkClose {
			lastContent = i
			firstContent = min(firstContent, i)
		}
	}
	lineStart := w.ctx == ctxPara
	linkStart := false // the previous token opened a link at a line start
	for i, t := range w.toks {
		opensLink := lineStart && t.kind == tkOpen && t.mark.Type == "link"
		switch t.kind {
		case tkText:
			w.b.WriteString(w.escape(i, lineStart, linkStart))
		case tkRaw:
			w.b.WriteString(t.s)
		case tkCode:
			w.b.WriteString(codeSpan(t.s, w.ctx == ctxCell))
		case tkOpen, tkClose:
			w.b.WriteString(w.delim(i))
		case tkBreak:
			// goldmark misreads "\\\" at a line end, so a break after
			// a backslash is written as <br> too.
			if w.ctx == ctxHeading || w.ctx == ctxCell ||
				i < firstContent || i > lastContent || w.lastRune() == '\\' {
				w.b.WriteString("<br>")
			} else {
				w.b.WriteString("\\\n")
				lineStart, linkStart = true, false
				continue
			}
		}
		lineStart, linkStart = false, opensLink
	}
	return w.b.String()
}

func (w *writer) lastRune() rune {
	s := w.b.String()
	if s == "" {
		return '\n'
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// firstRune is the first character token i will write.
func (w *writer) firstRune(i int) rune {
	if i >= len(w.toks) {
		return '\n'
	}
	t := w.toks[i]
	switch t.kind {
	case tkText, tkRaw:
		r, _ := utf8.DecodeRuneInString(t.s)
		return r
	case tkCode:
		return '`'
	case tkBreak:
		if w.ctx == ctxHeading || w.ctx == ctxCell {
			return '<'
		}
		return '\\'
	case tkOpen:
		return rune(w.openText(i)[0])
	case tkClose:
		return rune(w.closeText(i)[0])
	}
	return '\n'
}

func (w *writer) delim(i int) string {
	if w.toks[i].kind == tkOpen {
		return w.openText(i)
	}
	return w.closeText(i)
}

func (w *writer) openText(i int) string {
	m := w.toks[i].mark
	switch m.Type {
	case "strong":
		return "**"
	case "em":
		return w.emDelim(i)
	case "strike":
		return "~~"
	case "underline":
		return "<u>"
	case "subsup":
		return "<" + subsupTag(m) + ">"
	case "link":
		return "["
	}
	return ""
}

func (w *writer) closeText(i int) string {
	m := w.toks[i].mark
	switch m.Type {
	case "strong":
		return "**"
	case "em":
		return w.emDelim(w.toks[i].pair)
	case "strike":
		return "~~"
	case "underline":
		return "</u>"
	case "subsup":
		return "</" + subsupTag(m) + ">"
	case "link":
		href, _ := m.Attrs["href"].(string)
		s := "](" + linkDest(href, w.ctx == ctxCell)
		if title, ok := m.Attrs["title"].(string); ok && title != "" {
			s += ` "` + titleEscaper.Replace(title) + `"`
		}
		return s + ")"
	}
	return ""
}

func subsupTag(m Mark) string {
	if t, _ := m.Attrs["type"].(string); t == "sup" {
		return "sup"
	}
	return "sub"
}

// emDelim picks _ for emphasis unless a neighbor is a letter or digit,
// where only * works. The neighbors come from the tokens, not from what
// has been written so far, because lookahead asks before the text in
// front of the delimiter is written.
func (w *writer) emDelim(open int) string {
	before := w.rawEdge(open-1, true)
	after := '\n'
	if closeAt := w.toks[open].pair; closeAt > open && closeAt+1 < len(w.toks) {
		next := w.toks[closeAt+1]
		if next.kind == tkText || next.kind == tkRaw {
			after, _ = utf8.DecodeRuneInString(next.s)
		}
	}
	if isAlnum(before) || isAlnum(after) {
		return "*"
	}
	return "_"
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func codeSpan(s string, inCell bool) string {
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") &&
			strings.Trim(s, " ") != "") {
		pad = " "
	}
	if inCell {
		s = strings.ReplaceAll(s, "|", `\|`)
	}
	return fence + pad + s + pad + fence
}

var needsAngle = regexp.MustCompile(`[\s<>]`)

// linkDest writes a link destination so it parses back unchanged.
func linkDest(href string, inCell bool) string {
	esc := func(s string, chars string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case strings.IndexByte(chars, c) >= 0:
				b.WriteByte('\\')
			case c == '&' && entityAt(s[i:]):
				b.WriteByte('\\')
			case c == '|' && inCell:
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		return b.String()
	}
	if href == "" || needsAngle.MatchString(href) || !balanced(href) {
		return "<" + esc(strings.ReplaceAll(href, "\n", "%0A"), `\<>`) + ">"
	}
	return esc(href, `\`)
}

func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func entityAt(s string) bool {
	_, n := charRef([]byte(s))
	return n > 0
}

// escapeAlt escapes image alt and attachment labels, which Markdown
// parses as inline text. Underscores inside words (file names) are safe.
func escapeAlt(s string) string {
	rs := []rune(strings.ReplaceAll(s, "\n", " "))
	var b strings.Builder
	for i, r := range rs {
		esc := strings.ContainsRune("\\[]*`<~|", r)
		switch r {
		case '_':
			esc = i == 0 || i == len(rs)-1 || !isAlnum(rs[i-1]) ||
				!isAlnum(rs[i+1])
		case '&':
			esc = entityAt(string(rs[i:]))
		}
		if esc {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// emojiText is what an emoji shows: its text, with \uXXXX escapes (some
// stored text holds "\ud83d\udccb" for 📋) decoded, or its short name
// when the text is empty or holds an escape that names no character.
func emojiText(n *Node) string {
	t := n.attrStr("text")
	if strings.Contains(t, `\u`) {
		d, ok := decodeUEscapes(t)
		if !ok {
			return n.attrStr("shortName")
		}
		t = d
	}
	return cmp.Or(t, n.attrStr("shortName"))
}

// decodeUEscapes decodes the \uXXXX escapes in s, surrogate pairs
// included; ok is false when one is malformed or a lone surrogate.
func decodeUEscapes(s string) (string, bool) {
	var b strings.Builder
	hex := func(i int) (rune, bool) {
		if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], `\u`) {
			b.WriteByte(s[i])
			i++
			continue
		}
		r, ok := hex(i)
		if !ok {
			return "", false
		}
		i += 6
		if utf16.IsSurrogate(r) {
			lo, ok := hex(i)
			if r = utf16.DecodeRune(r, lo); !ok || r == utf8.RuneError {
				return "", false
			}
			i += 6
		}
		b.WriteRune(r)
	}
	return b.String(), true
}
