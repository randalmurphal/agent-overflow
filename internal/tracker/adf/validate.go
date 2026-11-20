package adf

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Validate checks doc against the ADF content rules Jira and Confluence
// enforce: allowed children, required and known attributes, attribute
// values, and which marks may combine where. It reports up to ten
// problems, each with a path like content[2].content[0].
func Validate(doc *Node) error {
	if doc == nil {
		return errors.New("adf: nil document")
	}
	v := validator{}
	if doc.Type != "doc" {
		v.fail(nil, "root is %q, want doc", doc.Type)
	} else if doc.Version != 1 {
		v.fail(nil, "doc version is %d, want 1", doc.Version)
	}
	v.node(doc, "", nil)
	if len(v.errs) == 0 {
		return nil
	}
	return errors.Join(v.errs...)
}

type validator struct {
	errs []error
}

// nodePath locates a node as its parent's path plus its index there;
// the root is nil. It is formatted only when a check fails.
type nodePath struct {
	parent *nodePath
	index  int
}

func (p *nodePath) String() string {
	if p == nil {
		return "doc"
	}
	var b strings.Builder
	p.write(&b)
	return b.String()
}

func (p *nodePath) write(b *strings.Builder) {
	if p.parent != nil {
		p.parent.write(b)
		b.WriteByte('.')
	}
	b.WriteString("content[" + strconv.Itoa(p.index) + "]")
}

func (v *validator) fail(path *nodePath, format string, args ...any) {
	if len(v.errs) >= 10 {
		return
	}
	v.errs = append(v.errs, fmt.Errorf("adf: %s: "+format,
		append([]any{path.String()}, args...)...))
}

func (v *validator) node(n *Node, parent string, path *nodePath) {
	if n == nil {
		v.fail(path, "nil node")
		return
	}
	s, ok := specs.get()[n.Type]
	if !ok {
		v.fail(path, "unknown node type %q", n.Type)
		return
	}
	if n.Version != 0 && n.Type != "doc" {
		v.fail(path, "%s cannot have a version", n.Type)
	}
	if n.Type != "text" && n.Text != "" {
		v.fail(path, "%s cannot have text", n.Type)
	}
	v.attrs(n, s, path)
	if n.Type == "text" {
		v.text(n, parent, path)
	} else {
		v.nodeMarks(n, parent, path)
	}
	if s.content == nil && len(n.Content) > 0 {
		v.fail(path, "%s cannot have content", n.Type)
	}
	if s.content != nil {
		v.content(n, s, parent, path)
	}
	for i, c := range n.Content {
		v.node(c, n.Type, &nodePath{path, i})
	}
}

func (v *validator) content(n *Node, s spec, parent string, path *nodePath) {
	min, max := s.min, s.max
	if n.Type == "layoutSection" && parent != "doc" {
		min, max = 0, 0 // only the root variant bounds its columns
	}
	if len(n.Content) < min {
		v.fail(path, "%s needs at least %d child nodes", n.Type, min)
	}
	if max > 0 && len(n.Content) > max {
		v.fail(path, "%s takes at most %d child nodes", n.Type, max)
	}
	for i, c := range n.Content {
		if c == nil {
			continue
		}
		if !allowed(n.Type, c.Type) {
			v.fail(path, "%s cannot contain %s", n.Type, c.Type)
			continue
		}
		if n.Type == "mediaSingle" && (i == 0) != (c.Type == "media") {
			v.fail(path, "mediaSingle holds one media then an optional "+
				"caption")
		}
	}
}

func (v *validator) attrs(n *Node, s spec, path *nodePath) {
	if s.noAttrs && len(n.Attrs) > 0 {
		v.fail(path, "%s cannot have attrs", n.Type)
		return
	}
	for k := range n.Attrs {
		if !slices.Contains(s.attrs, k) {
			v.fail(path, "%s has unknown attr %q", n.Type, k)
		}
	}
	for _, k := range s.required {
		if _, ok := n.Attrs[k]; !ok {
			v.fail(path, "%s needs attr %q", n.Type, k)
		}
	}
	if n.Type == "nestedExpand" && n.Attrs == nil {
		v.fail(path, "nestedExpand needs attrs")
	}
	v.attrValues(n, path)
}

var (
	hexColor    = lazyRegexp{pattern: `^#[0-9a-fA-F]{6}$`}
	statusColor = lazyRegexp{pattern: `^(neutral|purple|blue|red|yellow|green|#[0-9a-fA-F]{6})$`}
	layouts     = []string{
		"wide", "full-width", "center", "wrap-right", "wrap-left",
		"align-end", "align-start",
	}
)

// attrValues checks the attribute values the schema constrains.
func (v *validator) attrValues(n *Node, path *nodePath) {
	str := func(k string, nonEmpty bool) {
		x, ok := n.Attrs[k]
		if !ok {
			return
		}
		s, isStr := x.(string)
		if !isStr {
			v.fail(path, "%s attr %q must be a string", n.Type, k)
		} else if nonEmpty && s == "" {
			v.fail(path, "%s attr %q must not be empty", n.Type, k)
		}
	}
	enum := func(k string, vals ...string) {
		x, ok := n.Attrs[k]
		if !ok {
			return
		}
		if s, _ := x.(string); !slices.Contains(vals, s) {
			v.fail(path, "%s attr %q must be one of %s", n.Type, k,
				strings.Join(vals, ", "))
		}
	}
	num := func(k string, lo, hi float64) {
		if _, ok := n.Attrs[k]; !ok {
			return
		}
		f, ok := n.attrNum(k)
		if !ok || f < lo || f > hi {
			v.fail(path, "%s attr %q out of range", n.Type, k)
		}
	}
	const inf = 1e18
	switch n.Type {
	case "text":
		if n.Text == "" {
			v.fail(path, "text node needs non-empty text")
		}
	case "heading":
		num("level", 1, 6)
	case "orderedList":
		num("order", 0, inf)
	case "panel":
		enum("panelType", "info", "note", "tip", "warning", "error",
			"success", "custom")
	case "taskItem", "blockTaskItem":
		enum("state", "TODO", "DONE")
		str("localId", false)
	case "decisionItem", "decisionList", "taskList":
		str("localId", false)
		str("state", false)
	case "status":
		str("text", true)
		if c, _ := n.Attrs["color"].(string); !statusColor.get().MatchString(c) {
			v.fail(path, "status color %q is not allowed", c)
		}
	case "date":
		str("timestamp", true)
	case "mention":
		str("id", false)
		enum("userType", "DEFAULT", "SPECIAL", "APP")
	case "emoji":
		str("shortName", false)
	case "codeBlock":
		str("language", false)
	case "extension", "bodiedExtension", "inlineExtension":
		str("extensionKey", true)
		str("extensionType", true)
		enum("layout", "wide", "full-width", "default")
	case "layoutColumn":
		num("width", 0, 100)
	case "tableCell", "tableHeader":
		num("colspan", 0, inf)
		num("rowspan", 0, inf)
	case "table":
		enum("layout", "wide", "full-width", "center", "align-end",
			"align-start", "default")
		enum("displayMode", "default", "fixed")
	case "hardBreak":
		enum("text", "\n")
	case "inlineCard":
		if n.attr("url") == nil && n.attr("data") == nil {
			v.fail(path, "inlineCard needs url or data")
		}
	case "blockCard":
		if n.attr("url") == nil && n.attr("data") == nil &&
			n.attr("datasource") == nil {
			v.fail(path, "blockCard needs url, data or datasource")
		}
	case "embedCard":
		enum("layout", layouts...)
	case "mediaSingle":
		v.mediaSingle(n, path)
	case "media":
		v.media(n, path)
	case "mediaInline":
		enum("type", "link", "file", "image")
		str("id", true)
	}
}

func (v *validator) mediaSingle(n *Node, path *nodePath) {
	if n.Attrs == nil {
		return
	}
	if !slices.Contains(layouts, n.attrStr("layout")) {
		v.fail(path, "mediaSingle needs a valid layout")
	}
	switch n.attrStr("widthType") {
	case "", "percentage":
		if w, ok := n.attrNum("width"); ok && (w < 0 || w > 100) {
			v.fail(path, "mediaSingle percentage width out of range")
		}
	case "pixel":
		if _, ok := n.attrNum("width"); !ok {
			v.fail(path, "mediaSingle pixel widthType needs width")
		}
	default:
		v.fail(path, "mediaSingle widthType is not allowed")
	}
}

func (v *validator) media(n *Node, path *nodePath) {
	switch n.attrStr("type") {
	case "file", "link":
		if n.attrStr("id") == "" {
			v.fail(path, "media needs a non-empty id")
		}
		if _, ok := n.Attrs["collection"].(string); !ok {
			v.fail(path, "media needs a collection")
		}
		if n.attr("url") != nil {
			v.fail(path, "file media cannot have url")
		}
	case "external":
		if _, ok := n.Attrs["url"].(string); !ok {
			v.fail(path, "external media needs url")
		}
		for _, k := range []string{"id", "collection", "occurrenceKey"} {
			if n.attr(k) != nil {
				v.fail(path, "external media cannot have %s", k)
			}
		}
	default:
		v.fail(path, "media type must be file, link or external")
	}
}

func (v *validator) text(n *Node, parent string, path *nodePath) {
	if parent == "codeBlock" {
		if len(n.Marks) > 0 {
			v.fail(path, "code block text cannot have marks")
		}
		return
	}
	allowedSet := formattedMarks
	if hasMark(n, "code") {
		allowedSet = codeMarks
	}
	v.marks(n.Marks, allowedSet, path, "text")
}

func (v *validator) nodeMarks(n *Node, parent string, path *nodePath) {
	if len(n.Marks) == 0 {
		return
	}
	v.marks(n.Marks, nodeMarks(parent, n.Type), path, n.Type)
}

func (v *validator) marks(ms []Mark, allowedSet []string,
	path *nodePath, what string) {
	seen := map[string]bool{}
	for _, m := range ms {
		if !slices.Contains(allowedSet, m.Type) {
			if m.Type == "code" || hasCode(ms) {
				v.fail(path, "inline code cannot combine with %s",
					otherMark(ms, m))
			} else {
				v.fail(path, "%s cannot have the %s mark", what, m.Type)
			}
			continue
		}
		if seen[m.Type] && m.Type != "annotation" {
			v.fail(path, "%s has the %s mark twice", what, m.Type)
		}
		seen[m.Type] = true
		v.markAttrs(m, path)
	}
}

func hasCode(ms []Mark) bool {
	for _, m := range ms {
		if m.Type == "code" {
			return true
		}
	}
	return false
}

func otherMark(ms []Mark, m Mark) string {
	if m.Type != "code" {
		return m.Type
	}
	for _, o := range ms {
		if !slices.Contains(codeMarks, o.Type) {
			return o.Type
		}
	}
	return "other marks"
}

func (v *validator) markAttrs(m Mark, path *nodePath) {
	s, ok := markSpecs.get()[m.Type]
	if !ok {
		v.fail(path, "unknown mark %q", m.Type)
		return
	}
	for k := range m.Attrs {
		if !slices.Contains(s.attrs, k) {
			v.fail(path, "%s mark has unknown attr %q", m.Type, k)
		}
	}
	for _, k := range s.required {
		if _, ok := m.Attrs[k]; !ok {
			v.fail(path, "%s mark needs attr %q", m.Type, k)
		}
	}
	get := func(k string) string {
		s, _ := m.Attrs[k].(string)
		return s
	}
	switch m.Type {
	case "link":
		if _, ok := m.Attrs["href"].(string); !ok {
			v.fail(path, "link href must be a string")
		}
	case "textColor", "backgroundColor":
		if !hexColor.get().MatchString(get("color")) {
			v.fail(path, "%s color must be #rrggbb", m.Type)
		}
	case "subsup":
		if t := get("type"); t != "sub" && t != "sup" {
			v.fail(path, "subsup type must be sub or sup")
		}
	case "alignment":
		if a := get("align"); a != "center" && a != "end" {
			v.fail(path, "alignment must be center or end")
		}
	case "breakout":
		if md := get("mode"); md != "wide" && md != "full-width" {
			v.fail(path, "breakout mode must be wide or full-width")
		}
	}
}
