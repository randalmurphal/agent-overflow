// Package adf converts between Atlassian Document Format (the JSON tree
// Jira and Confluence store rich text in) and Markdown.
//
// Read path: ToMarkdown renders every node as readable Markdown, as
// faithfully as Markdown allows. Write path: FromMarkdown builds valid ADF
// from GitHub-flavored Markdown plus a few extensions (alerts, mentions,
// <details>, media refs). Edit path: Edit converts new Markdown and reuses
// the original ADF node for every top-level block the edit left unchanged,
// so content Markdown cannot express (status lozenges, column widths,
// macros, layouts) survives edits that don't touch it.
package adf

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// Node is one ADF node. The JSON shape is ADF's own.
type Node struct {
	Type    string         `json:"type"`
	Version int            `json:"version,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []*Node        `json:"content,omitempty"`
	Marks   []Mark         `json:"marks,omitempty"`
	Text    string         `json:"text,omitempty"`
}

// Mark is one ADF text mark.
type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Resolver supplies lookups the write path needs. Either method may
// return an error, which becomes a conversion error naming the line.
type Resolver interface {
	// Mention resolves the inside of @[...] (a name, an email or "me") to
	// an account ID and display name.
	Mention(ctx context.Context, who string) (id, display string, err error)
	// Upload uploads a local file for ![alt](path) and returns its media
	// file ID.
	Upload(ctx context.Context, path string) (mediaID string, err error)
}

// CollectionResolver is optionally implemented by a Resolver to name the
// media collection that file nodes use: Jira wants "" (the default),
// Confluence "contentId-<pageId>". It applies to uploads and media:ID
// references alike.
type CollectionResolver interface {
	Collection() string
}

// Warning is a non-fatal conversion note, e.g. a mark Jira cannot combine
// or a replaced block that held content Markdown cannot express.
type Warning struct {
	// Line is the 1-based Markdown line, or 0 when not tied to one.
	Line int
	Msg  string
	// Flattens marks an Edit warning that loses content: a changed block
	// that held a column layout, a macro with a body or a custom panel (the
	// edit rewrites the container as plain Markdown blocks), or a content
	// node the Markdown still shows (a lozenge, a mention, a header cell).
	// Callers should refuse such an edit unless the user accepts the loss.
	// Lost dress (a color, an alignment, a width) warns without it.
	Flattens bool
}

// String formats w as "line N: msg", or just msg without a line.
func (w Warning) String() string {
	if w.Line == 0 {
		return w.Msg
	}
	return "line " + strconv.Itoa(w.Line) + ": " + w.Msg
}

// Blank reports whether doc holds nothing a reader sees: nil, empty, or
// only paragraphs of line breaks and blank space. A body of "<br>" or of
// a byte-order mark alone is blank.
func Blank(doc *Node) bool {
	blank := true
	if doc == nil {
		return true
	}
	walk(doc, func(n, _ *Node) {
		switch n.Type {
		case "doc", "paragraph", "hardBreak":
		case "text":
			blank = blank && strings.TrimSpace(strings.ReplaceAll(n.Text, "\ufeff", "")) == ""
		default:
			blank = false
		}
	})
	return blank
}

// Equal reports whether a and b write the same ADF JSON. A nil doc equals
// an empty one, so callers can tell when an edit would change nothing.
func Equal(a, b *Node) bool {
	if a == nil {
		a = Doc()
	}
	if b == nil {
		b = Doc()
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// ReplaceMediaIDs sets the id of every media node in doc whose id is a key
// of ids to its value, and reports how many it changed. Callers that hold
// uploads back until a write is checked use it to swap placeholders for
// the uploaded files' IDs.
func ReplaceMediaIDs(doc *Node, ids map[string]string) int {
	n := 0
	if doc == nil {
		return 0
	}
	walk(doc, func(m, _ *Node) {
		if m.Type != "media" {
			return
		}
		if id, ok := ids[m.attrStr("id")]; ok {
			m.Attrs["id"] = id
			n++
		}
	})
	return n
}
