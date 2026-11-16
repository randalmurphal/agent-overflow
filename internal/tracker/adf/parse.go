package adf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// FromMarkdown converts GitHub-flavored Markdown to an ADF doc that
// passes Validate. Syntax with no ADF equivalent is an error naming the
// 1-based line; content ADF can't hold where it was written (a heading
// in a list item, say) is reshaped and reported as a Warning. r resolves
// @[mentions] and uploads local images; with a nil r those are errors.
// If r implements CollectionResolver, media file nodes use its
// collection. Resolver errors are wrapped, so errors.Is sees them.
func FromMarkdown(ctx context.Context, md string, r Resolver) (*Node,
	[]Warning, error) {
	doc, ws, err := fromMarkdown(ctx, md, r)
	return doc, publicWarnings(ws), publicErr(err)
}

func fromMarkdown(ctx context.Context, md string, r Resolver) (*Node,
	[]Warning, error) {
	md = lineFeeds(md)
	c := newConverter(ctx, md, r, true)
	parsed, err := c.document()
	if err != nil {
		return nil, nil, err
	}
	var blocks []*Node
	for _, b := range parsed {
		blocks = append(blocks, c.settle(b)...)
	}
	newAt := make([]bool, len(blocks))
	for i := range newAt {
		newAt[i] = true
	}
	if err := c.resolvePending(newAt, blocks, nil); err != nil {
		return nil, nil, err
	}
	doc := finish(Doc(blocks...))
	if err := Validate(doc); err != nil {
		return nil, nil, fmt.Errorf("adf: converter built invalid ADF: %w",
			err)
	}
	return doc, c.st.warns, nil
}

// Internal node types. They never leave the package: finish strips
// markers, and the others are consumed while building.
const (
	tMarker       = "\x00marker"   // an HTML comment, kept for Edit
	tDetailsOpen  = "\x00details"  // <details>, until grouped
	tDetailsClose = "\x00/details" // </details>, until grouped
	tImage        = "\x00image"    // inline wrapper for a mediaSingle
	pendingID     = "\x00pending"  // unresolved mention or upload
	attrWho       = "\x00who"
	attrPath      = "\x00path"
	attrMacroID   = "\x00mid" // the ID a macro's markers were written with
)

// lineError is a conversion error tied to a Markdown line. When it comes
// from the Resolver it wraps that error for errors.Is and errors.As.
type lineError struct {
	line int
	msg  string
	err  error
}

func (e *lineError) Error() string {
	if e.line <= 0 {
		return e.msg
	}
	return fmt.Sprintf("line %d: %s", e.line, e.msg)
}

func (e *lineError) Unwrap() error { return e.err }

func errAt(line int, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	return &lineError{line: line, msg: err.Error(), err: errors.Unwrap(err)}
}

// converter turns one goldmark parse into ADF. Sub-parses (Markdown
// found inside an HTML block) get their own converter sharing st.
type converter struct {
	ctx     context.Context
	r       Resolver
	deferR  bool // leave mentions and uploads pending (Edit, checks)
	src     []byte
	lineOff int
	starts  []int
	st      *convState
}

type convState struct {
	warns    []Warning
	lines    map[*Node]int
	mentions map[string][2]string
	uploads  map[string]string
	src      string // the whole Markdown, which seeds localIds
	seed     string // set by the first newID
	ids      int
	coll     string // media collection for file nodes
	// built marks containers made from markers alone (a macro or a
	// custom panel), which need the original's attributes to be kept.
	built map[*Node]bool
	// decisions reads "- Decision: " lists back as decision lists, for an
	// edit of a document that has them.
	decisions bool
	// macros are the original's macro IDs by key, for an edit: a marker
	// matching none of them is refused (see macroMiss).
	macros map[string][]string
}

// lineFeeds is md without a leading byte-order mark, which would make a
// first "## Heading" text, and with each line ending, CRLF or a lone CR,
// as LF: a CR left in text would be stored there, and the view shows it
// as a space.
// Every reading of Markdown takes it, so an original's code block that
// holds CRLF still reads as md's and stays as it was.
func lineFeeds(md string) string {
	md = strings.TrimPrefix(md, "\ufeff") // a byte-order mark is no text
	if !strings.Contains(md, "\r") {
		return md
	}
	return strings.ReplaceAll(strings.ReplaceAll(md, "\r\n", "\n"), "\r", "\n")
}

func newConverter(ctx context.Context, md string, r Resolver,
	deferR bool) *converter {
	md = lineFeeds(md)
	st := &convState{
		lines:    map[*Node]int{},
		mentions: map[string][2]string{},
		uploads:  map[string]string{},
		src:      md,
		built:    map[*Node]bool{},
	}
	if cr, ok := r.(CollectionResolver); ok {
		st.coll = cr.Collection()
	}
	return newSubConverter(ctx, md, r, deferR, st, 0)
}

func newSubConverter(ctx context.Context, md string, r Resolver,
	deferR bool, st *convState, lineOff int) *converter {
	src := []byte(md)
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &converter{ctx: ctx, r: r, deferR: deferR, src: src,
		lineOff: lineOff, starts: starts, st: st}
}

var (
	mdOnce   sync.Once
	mdParser parser.Parser
)

func markdownParser() parser.Parser {
	mdOnce.Do(func() {
		md := goldmark.New(
			goldmark.WithExtensions(extension.Table,
				extension.Strikethrough, extension.TaskList,
				extension.Linkify, extension.Footnote),
			goldmark.WithParserOptions(parser.WithInlineParsers(
				util.Prioritized(mentionParser{}, 500))),
		)
		mdParser = md.Parser()
	})
	return mdParser
}

// document parses the whole source and returns top-level blocks, still
// holding markers and pending references.
func (c *converter) document() ([]*Node, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	root := markdownParser().Parse(text.NewReader(c.src))
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	blocks, err := c.blocks(root)
	if err != nil {
		return nil, err
	}
	doc := &Node{Type: "doc", Content: blocks}
	if err := c.fit(doc); err != nil {
		return nil, err
	}
	if c.st.decisions {
		c.decisionize(doc)
	}
	return doc.Content, nil
}

// decisionize turns each bullet list whose every item is one paragraph
// starting "Decision: ", as ToMarkdown writes a decision list, back into
// a decision list, where its parent may hold one.
func (c *converter) decisionize(root *Node) {
	walk(root, func(p, _ *Node) {
		if !allowed(p.Type, "decisionList") {
			return
		}
		for i, b := range p.Content {
			if d := c.decisionList(b); d != nil {
				p.Content[i] = d
			}
		}
	})
}

func (c *converter) decisionList(b *Node) *Node {
	if b.Type != "bulletList" || len(b.Content) == 0 {
		return nil
	}
	var items []*Node
	for _, it := range b.Content {
		if len(it.Content) != 1 || it.Content[0].Type != "paragraph" {
			return nil
		}
		inl := it.Content[0].Content
		if len(inl) == 0 || inl[0].Type != "text" || len(inl[0].Marks) > 0 ||
			!strings.HasPrefix(inl[0].Text, "Decision: ") {
			return nil
		}
		first := *inl[0]
		first.Text = strings.TrimPrefix(first.Text, "Decision: ")
		content := append([]*Node{&first}, inl[1:]...)
		if first.Text == "" {
			content = inl[1:]
		}
		items = append(items, c.at(&Node{Type: "decisionItem",
			Attrs:   map[string]any{"localId": c.newID(), "state": "DECIDED"},
			Content: content}, c.lineFor(it)))
	}
	return c.at(&Node{Type: "decisionList", Attrs: map[string]any{"localId": c.newID()},
		Content: items}, c.lineFor(b))
}

func (c *converter) lineAt(off int) int {
	n := sort.Search(len(c.starts), func(i int) bool {
		return c.starts[i] > off
	})
	return c.lineOff + n
}

func (c *converter) lineOf(n gast.Node) int {
	if p := n.Pos(); p >= 0 {
		return c.lineAt(p)
	}
	if n.Type() == gast.TypeBlock && n.Lines().Len() > 0 {
		return c.lineAt(n.Lines().At(0).Start)
	}
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		if l := c.lineOf(ch); l > 0 {
			return l
		}
	}
	return 0
}

func (c *converter) warn(line int, format string, args ...any) {
	w := Warning{Line: line, Msg: fmt.Sprintf(format, args...)}
	for _, o := range c.st.warns {
		if o == w {
			return
		}
	}
	c.st.warns = append(c.st.warns, w)
}

// at records the Markdown line a built node came from.
func (c *converter) at(n *Node, line int) *Node {
	c.st.lines[n] = line
	return n
}

func (c *converter) lineFor(n *Node) int {
	return c.st.lines[n]
}

// newID makes a localId. IDs are derived from the source so output is
// deterministic; Edit ignores localIds when comparing.
func (c *converter) newID() string {
	if c.st.seed == "" {
		sum := sha256.Sum256([]byte(c.st.src))
		c.st.seed = hex.EncodeToString(sum[:8])
	}
	c.st.ids++
	sum := sha256.Sum256([]byte(c.st.seed + "/" + strconv.Itoa(c.st.ids)))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] +
		"-" + h[20:32]
}

// mention resolves @[who].
func (c *converter) mention(who string, line int) (*Node, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	who = strings.TrimSpace(who)
	if who == "" {
		return nil, errAt(line, "@[] needs a name, email or \"me\"")
	}
	if id, ok := strings.CutPrefix(who, "accountid:"); ok {
		if strings.TrimSpace(id) == "" {
			return nil, errAt(line, "accountid: reference needs an ID")
		}
		return &Node{Type: "mention", Attrs: map[string]any{"id": id}}, nil
	}
	if c.deferR {
		return &Node{Type: "mention", Attrs: map[string]any{
			"id": pendingID, attrWho: who,
		}}, nil
	}
	p, ok := c.st.mentions[who]
	if !ok {
		if c.r == nil {
			return nil, errAt(line, "@[%s]: mentions need a user lookup, "+
				"which is not available here", who)
		}
		id, display, err := c.r.Mention(c.ctx, who)
		if err != nil {
			return nil, errAt(line, "@[%s]: %w", who, err)
		}
		p = [2]string{id, display}
		c.st.mentions[who] = p
	}
	return &Node{Type: "mention", Attrs: map[string]any{
		"id": p[0], "text": "@" + p[1],
	}}, nil
}

// upload turns a local image path into a media file ID.
func (c *converter) upload(path string, line int) (string, error) {
	if err := c.ctx.Err(); err != nil {
		return "", err
	}
	if c.deferR {
		return pendingID, nil
	}
	if id, ok := c.st.uploads[path]; ok {
		return id, nil
	}
	if c.r == nil {
		return "", errAt(line, "image %s: local files need an upload, "+
			"which is not available here", path)
	}
	id, err := c.r.Upload(c.ctx, path)
	if err != nil {
		return "", errAt(line, "image %s: %w", path, err)
	}
	c.st.uploads[path] = id
	return id, nil
}

// finish strips internal markers and gives every container that must
// not be empty an empty paragraph.
func finish(n *Node) *Node {
	if len(n.Content) == 0 {
		if n.Type == "doc" {
			return n
		}
	}
	out := n.Content[:0]
	for _, ch := range n.Content {
		if ch.Type == tMarker {
			continue
		}
		out = append(out, finish(ch))
	}
	n.Content = out
	if isInlineParent(n.Type) {
		n.Content = mergeText(n.Content)
	}
	if len(n.Content) == 0 && n.Type != "doc" && specs[n.Type].min > 0 &&
		allowed(n.Type, "paragraph") {
		n.Content = []*Node{{Type: "paragraph"}}
	}
	if len(n.Content) == 0 {
		n.Content = nil
	}
	return n
}
