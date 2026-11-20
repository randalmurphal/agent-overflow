package adf

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// escLevel selects how much text escaping to apply. Zero is the minimal
// context-aware escaping; each bit escapes one category everywhere.
type escLevel uint16

const (
	escEmph    escLevel = 1 << iota // every * _ ~
	escBracket                      // every [ ]
	escTick                         // every backtick
	escHTML                         // every < > &
	escLinkify                      // anything GFM might autolink
	escLinks                        // links as [text](url), never bare
	escLine                         // any punctuation starting a line
	escSlash                        // every backslash
	escFull                         // every ASCII punctuation character
)

const escAll = escEmph | escBracket | escTick | escHTML | escLinkify |
	escLinks | escLine | escSlash

// escLadder is the order renderInline tries levels in when minimal
// escaping misparses: each category alone, then all of them.
var escLadder = []escLevel{
	escEmph, escBracket, escTick, escHTML, escLinkify, escLinks, escLine,
	escSlash, escAll,
}

func isSpaceRune(r rune) bool {
	return r == '\n' || unicode.IsSpace(r)
}

func isPunctRune(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func hasASCIIPunct(s string) bool {
	for i := 0; i < len(s); i++ {
		if isASCIIPunct(s[i]) {
			return true
		}
	}
	return false
}

// delimSlot indexes writer.runs by emphasis character: * _ ~.
func delimSlot(r rune) int {
	switch r {
	case '*':
		return 0
	case '_':
		return 1
	}
	return 2
}

// canDelim reports whether a run of c between before and after could
// open or close emphasis under CommonMark's flanking rules.
func canDelim(c, before, after rune) bool {
	left := !isSpaceRune(after) &&
		(!isPunctRune(after) || isSpaceRune(before) || isPunctRune(before))
	right := !isSpaceRune(before) &&
		(!isPunctRune(before) || isSpaceRune(after) || isPunctRune(after))
	if c == '_' {
		return left && (!right || isPunctRune(before)) ||
			right && (!left || isPunctRune(after))
	}
	return left || right
}

// rawEdge approximates token i's first (or last) character before it is
// written, for counting delimiter runs.
func (w *writer) rawEdge(i int, last bool) rune {
	if i < 0 || i >= len(w.toks) {
		return '\n'
	}
	t := w.toks[i]
	switch t.kind {
	case tkText, tkRaw:
		if t.s == "" {
			return '\n'
		}
		if last {
			r, _ := utf8.DecodeLastRuneInString(t.s)
			return r
		}
		r, _ := utf8.DecodeRuneInString(t.s)
		return r
	case tkCode:
		return '`'
	case tkBreak:
		return '\n'
	}
	return '*'
}

// countRuns counts, per emphasis character, the runs in text that could
// act as delimiters, plus our own delimiters using that character. A
// lone run can't pair with anything, so it needs no escape.
func (w *writer) countRuns() {
	w.runs = [3]int{}
	for i, t := range w.toks {
		switch t.kind {
		case tkText:
			if !strings.ContainsAny(t.s, "*_~`") {
				continue
			}
			rs := []rune(t.s)
			for j := 0; j < len(rs); j++ {
				r := rs[j]
				if r == '`' {
					w.tick++
				}
				if r != '*' && r != '_' && r != '~' {
					continue
				}
				k := j
				for k < len(rs) && rs[k] == r {
					k++
				}
				before, after := w.rawEdge(i-1, true), w.rawEdge(i+1, false)
				if j > 0 {
					before = rs[j-1]
				}
				if k < len(rs) {
					after = rs[k]
				}
				if canDelim(r, before, after) {
					w.runs[delimSlot(r)]++
				}
				j = k - 1
			}
		case tkCode:
			w.tick += 2
		case tkOpen:
			switch t.mark.Type {
			case "strong":
				w.runs[0] += 2
			case "em":
				w.runs[0] += 2
				w.runs[1] += 2
			case "strike":
				w.runs[2] += 2
			}
		}
	}
}

// afterText approximates what follows token i, for lookahead.
func (w *writer) afterText(i, limit int) string {
	var b strings.Builder
	for j := i + 1; j < len(w.toks) && b.Len() < limit; j++ {
		t := w.toks[j]
		switch t.kind {
		case tkText, tkRaw:
			b.WriteString(t.s)
		case tkCode:
			b.WriteString("`" + t.s + "`")
		case tkBreak:
			b.WriteString("\n")
		case tkOpen:
			if t.mark.Type == "link" {
				b.WriteString("[")
			} else {
				b.WriteString("*")
			}
		case tkClose:
			if t.mark.Type == "link" {
				b.WriteString("](")
			} else {
				b.WriteString("*")
			}
		}
	}
	return b.String()
}

var (
	reATX      = lazyRegexp{pattern: `^#{1,6}(\s|$)`}
	reBullet   = lazyRegexp{pattern: `^[-+*](\s|$)`}
	reOrdered  = lazyRegexp{pattern: `^[0-9]{1,9}([.)])(\s|$)`}
	reFence    = lazyRegexp{pattern: "^(```|~~~)"}
	reThematic = lazyRegexp{pattern: `^([-*_])([ \t]*([-*_]))*[ \t]*$`}
	reSetext   = lazyRegexp{pattern: `^(=+|-+)[ \t]*$`}
	// goldmark reads "[x]" at an item start as a task box whatever
	// follows it: "[x]foo" and "[x](url)" are tasks too.
	reTaskBox = lazyRegexp{pattern: `^\[[ xX]\]`}
	reRefDef  = lazyRegexp{pattern: `^\[[^\]]*\]:`}
)

// lineStart marks characters that would start block syntax when the
// text begins a line.
func (w *writer) lineStart(i int, rs []rune, esc []bool) {
	if len(rs) == 0 {
		return
	}
	s := string(rs)
	line := s + strings.SplitN(w.afterText(i, 120), "\n", 2)[0]
	if w.lvl&escLine != 0 && rs[0] < 128 && isASCIIPunct(byte(rs[0])) {
		esc[0] = true
	}
	switch {
	case reATX.get().MatchString(line), rs[0] == '>', reBullet.get().MatchString(line),
		reFence.get().MatchString(line), reSetext.get().MatchString(line),
		reTaskBox.get().MatchString(line), strings.HasPrefix(line, "[!"),
		reRefDef.get().MatchString(line):
		esc[0] = true
	case thematic(line):
		esc[0] = true
	}
	if m := reOrdered.get().FindStringSubmatchIndex(line); m != nil &&
		m[2] < len(rs) {
		esc[m[2]] = true
	}
}

var blankRemover = lazy[*strings.Replacer]{build: func() *strings.Replacer {
	return strings.NewReplacer(" ", "", "\t", "")
}}

func thematic(line string) bool {
	if !reThematic.get().MatchString(line) {
		return false
	}
	t := blankRemover.get().Replace(line)
	return len(t) >= 3 && strings.Count(t, t[:1]) == len(t)
}

func linkifyBoundary(r rune) bool {
	return r == '\n' || r == ' ' || r == '\t' || r == '*' || r == '_' ||
		r == '~' || r == '('
}

func isASCIILetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isEmailLocal(r rune) bool {
	return isASCIILetter(r) || r >= '0' && r <= '9' ||
		strings.ContainsRune(".!#$%&'*+/=?^_`{|}~-", r)
}

// escape renders text token i with the escapes its context needs.
// afterLinkStart means a link opened the line just before it.
func (w *writer) escape(i int, atLineStart, afterLinkStart bool) string {
	t := w.toks[i]
	if afterLinkStart && w.taskLabel(i) {
		return "&#" + strconv.Itoa(int(t.s[0])) + ";" // [x](url) reads as a task
	}
	if !hasASCIIPunct(t.s) && utf8.ValidString(t.s) {
		return t.s // every escape lands on ASCII punctuation
	}
	rs := []rune(t.s)
	esc := make([]bool, len(rs))
	prev, next := w.lastRune(), w.firstRune(i+1)
	at := func(j int) rune {
		switch {
		case j < 0:
			return prev
		case j >= len(rs):
			return next
		}
		return rs[j]
	}
	if atLineStart {
		w.lineStart(i, rs, esc)
	}
	if afterLinkStart && w.alertLabel(i) {
		esc[0] = true
	}
	if w.ctx == ctxHeading && w.lastContent(i) {
		k := len(rs)
		for k > 0 && rs[k-1] == '#' {
			k--
		}
		if k < len(rs) && isSpaceRune(at(k-1)) {
			esc[k] = true
		}
	}
	full := w.lvl&escFull != 0
	for j := 0; j < len(rs); j++ {
		r := rs[j]
		if r >= utf8.RuneSelf || !isASCIIPunct(byte(r)) {
			continue
		}
		switch {
		case full:
			esc[j] = true
		case r == '*' || r == '_' || r == '~':
			k := j
			for k < len(rs) && rs[k] == r {
				k++
			}
			if esc[j] || w.escapeRun(r, k-j, at(j-1), at(k)) {
				for x := j; x < k; x++ {
					esc[x] = true
				}
			}
			j = k - 1
		default:
			esc[j] = esc[j] || w.escapeChar(i, rs, j, at)
		}
	}
	n := len(t.s)
	for _, e := range esc {
		if e {
			n++
		}
	}
	// A ~ against a strike delimiter would join its run (~~~~~), and a
	// backslash does not stop goldmark pairing it; an entity does.
	ent := make([]bool, len(rs))
	for j := 0; j < len(rs) && rs[j] == '~' && w.strikeAt(i-1); j++ {
		ent[j] = true
	}
	for j := len(rs) - 1; j >= 0 && rs[j] == '~' && w.strikeAt(i+1); j-- {
		ent[j] = true
	}
	var b strings.Builder
	b.Grow(n)
	for j, r := range rs {
		switch {
		case ent[j]:
			b.WriteString("&#126;")
		case esc[j]:
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// strikeAt reports whether token i is a strike delimiter.
func (w *writer) strikeAt(i int) bool {
	return i >= 0 && i < len(w.toks) && (w.toks[i].kind == tkOpen ||
		w.toks[i].kind == tkClose) && w.toks[i].mark.Type == "strike"
}

// taskLabel reports whether text token i is a whole link text of one x
// or space, which at an item start reads as a task box.
func (w *writer) taskLabel(i int) bool {
	s := w.toks[i].s
	return (s == "x" || s == "X" || s == " ") && i+1 < len(w.toks) &&
		w.toks[i+1].kind == tkClose && w.toks[i+1].mark.Type == "link"
}

// alertLabel reports whether text token i is a whole link text like
// "!NOTE", which at the start of a quote reads as a GitHub alert.
func (w *writer) alertLabel(i int) bool {
	s := w.toks[i].s
	if len(s) < 2 || s[0] != '!' || i+1 == len(w.toks) ||
		w.toks[i+1].kind != tkClose || w.toks[i+1].mark.Type != "link" {
		return false
	}
	for j := 1; j < len(s); j++ {
		if !isASCIILetter(rune(s[j])) {
			return false
		}
	}
	return true
}

// escapeRun reports whether a run of n emphasis characters r between
// before and after needs escaping: when it could pair with another run
// or with our own delimiters, or when it would fence code.
func (w *writer) escapeRun(r rune, n int, before, after rune) bool {
	switch {
	case w.lvl&escEmph != 0:
		return true
	case canDelim(r, before, after) &&
		(w.runs[delimSlot(r)] >= 2 || before == r || after == r):
		return true
	}
	return r == '~' && n >= 3
}

// escapeChar reports whether punctuation rs[j] of text token i needs a
// backslash where it stands; at reads neighbors across token edges.
func (w *writer) escapeChar(i int, rs []rune, j int,
	at func(int) rune) bool {
	t := w.toks[i]
	p, n := at(j-1), at(j+1)
	last := j == len(rs)-1
	switch rs[j] {
	case '\\':
		return w.lvl&escSlash != 0 || last || n < 128 && isASCIIPunct(byte(n))
	case '`':
		return w.lvl&escTick != 0 || w.tick >= 2
	case '[':
		return w.lvl&escBracket != 0 || t.inLink || p == '!' || p == '@' ||
			n == '^' || w.closesAsLink(i, rs[j+1:])
	case ']':
		return w.lvl&escBracket != 0 || t.inLink
	case '<':
		return w.lvl&escHTML != 0 || n == '!' || n == '?' ||
			readsAsMarkup(window(rs, j+1, markupLookahead))
	case '>':
		return w.lvl&escHTML != 0
	case '&':
		return w.lvl&escHTML != 0 || entityAt(window(rs, j, entityLookahead))
	case '|':
		return w.ctx == ctxCell
	case '!':
		return n == '[' && last
	case '@':
		return n == '[' && last || !t.inLink &&
			(w.lvl&escLinkify != 0 || w.linkifyEmail(rs, j, at))
	case ':':
		return !t.inLink && w.linkifyScheme(rs, j, at)
	case '.':
		return !t.inLink && w.linkifyWWW(rs, j, at)
	}
	return false
}

// lastContent reports whether no text, code or markup follows token i.
func (w *writer) lastContent(i int) bool {
	for _, t := range w.toks[i+1:] {
		if t.kind == tkText || t.kind == tkRaw || t.kind == tkCode {
			return false
		}
	}
	return true
}

// The lookaheads escapeChar reads past a character, in runes, so text
// with much punctuation escapes in linear time: an entity is at most 32
// characters, and a tag or autolink longer than markupLookahead, which
// the parse check in renderInline still catches, is left to it.
const (
	entityLookahead = 40
	markupLookahead = 1024
	linkLookahead   = 4096
)

// window is the text of rs from from on, at most n runes of it.
func window(rs []rune, from, n int) string {
	return string(rs[from:min(len(rs), from+n)])
}

// closesAsLink reports whether a [ would pair with a later ] that is
// followed by ( or [, making a link. A ] further than linkLookahead runes
// counts as one that does: the backslash is safe either way.
func (w *writer) closesAsLink(i int, rest []rune) bool {
	j := 0
	for ; j < len(rest); j++ {
		if j >= linkLookahead {
			return true
		}
		switch rest[j] {
		case '\\':
			j++
		case ']':
			if j+1 < len(rest) {
				return rest[j+1] == '(' || rest[j+1] == '['
			}
			s := w.afterText(i, 400)
			return s != "" && (s[0] == '(' || s[0] == '[')
		}
	}
	// The text after this token, past what an escape at the end skipped.
	s := w.afterText(i, 400)
	for k := j - len(rest); k < len(s); k++ {
		switch s[k] {
		case '\\':
			k++
		case ']':
			return k+1 < len(s) && (s[k+1] == '(' || s[k+1] == '[')
		}
	}
	return false
}

func (w *writer) linkifyScheme(rs []rune, j int, at func(int) rune) bool {
	if w.lvl&escLinkify != 0 {
		return j+2 < len(rs)+1 && at(j+1) == '/'
	}
	k := j
	for k > 0 && isASCIILetter(rs[k-1]) {
		k--
	}
	switch string(rs[k:j]) {
	case "http", "https", "ftp":
		return linkifyBoundary(at(k - 1))
	}
	return false
}

func (w *writer) linkifyWWW(rs []rune, j int, at func(int) rune) bool {
	if j < 3 || string(rs[j-3:j]) != "www" {
		return false
	}
	return w.lvl&escLinkify != 0 || linkifyBoundary(at(j-4))
}

func (w *writer) linkifyEmail(rs []rune, j int, at func(int) rune) bool {
	k := j
	for k > 0 && isEmailLocal(rs[k-1]) {
		k--
	}
	if k == j || !linkifyBoundary(at(k-1)) {
		return false
	}
	dot := false
	for x := j + 1; x < len(rs); x++ {
		r := rs[x]
		if r == '.' {
			dot = true
			continue
		}
		if !isASCIILetter(r) && (r < '0' || r > '9') && r != '-' &&
			r != '_' {
			break
		}
	}
	return dot
}

// keptTags are the inline HTML tags FromMarkdown reads as marks; any other
// tag-shaped text (List<String>) is read back as literal text.
var keptTags = []string{"u", "ins", "sub", "sup", "br"}

// readsAsMarkup reports whether rest, the text after a '<', would be read
// as a kept tag or an autolink rather than as literal text. The parse
// check in renderInline still covers anything this misses, such as an
// HTML block at a line start.
func readsAsMarkup(rest string) bool {
	name := strings.TrimPrefix(rest, "/")
	end := 0
	for end < len(name) && isASCIILetter(rune(name[end])) {
		end++
	}
	if end == 0 {
		return false
	}
	if end < len(name) {
		switch c := name[end]; {
		case c == ':', c == '+', c == '.', c == '-' || c >= '0' && c <= '9':
			return true // a URI scheme: <https://...>
		case c == '>', c == '/', c == ' ', c == '\t', c == '\n':
			for _, t := range keptTags {
				if strings.EqualFold(name[:end], t) {
					return true
				}
			}
		}
	}
	gt := strings.IndexByte(rest, '>')
	return gt > 0 && strings.Contains(rest[:gt], "@") // <a@b.c> mail autolink
}
