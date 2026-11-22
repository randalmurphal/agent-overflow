package adf

// Content rules derived from Atlassian's published ADF JSON schema
// (@atlaskit/adf-schema 57.7.21, testdata/schema-full.json). Validate
// enforces them and the write path shapes its output to fit them;
// rules_test.go checks the table against the schema itself.

import "slices"

type spec struct {
	content  []string // allowed child types; nil means a leaf
	min, max int      // bounds on len(content); max 0 means unbounded
	attrs    []string // allowed attribute keys
	required []string // required attribute keys
	marks    []string // node marks allowed in every context
	noAttrs  bool     // the node takes no attrs at all
}

var inlineTypes = []string{
	"text", "date", "emoji", "hardBreak", "inlineCard", "mention",
	"placeholder", "status", "inlineExtension", "mediaInline",
}

var nonNestable = []string{
	"paragraph", "panel", "blockquote", "orderedList", "bulletList",
	"rule", "heading", "codeBlock", "mediaGroup", "mediaSingle",
	"decisionList", "taskList", "table", "blockCard", "embedCard",
	"extension",
}

var extAttrs = []string{
	"extensionKey", "extensionType", "parameters", "text", "layout",
	"localId",
}

var mediaMarks = []string{"dataConsumer", "link", "annotation", "border"}

var specs = lazy[map[string]spec]{build: func() map[string]spec {
	return map[string]spec{
		"doc": {content: []string{
			"blockCard", "codeBlock", "mediaSingle", "paragraph", "taskList",
			"orderedList", "bulletList", "blockquote", "decisionList",
			"embedCard", "extension", "heading", "mediaGroup", "rule",
			"panel", "table", "bodiedExtension", "expand", "layoutSection",
			"syncBlock", "bodiedSyncBlock",
		}, noAttrs: true},
		"paragraph": {content: inlineTypes, attrs: []string{"localId"}},
		"heading": {content: inlineTypes,
			attrs: []string{"level", "localId"}, required: []string{"level"}},
		"text":      {noAttrs: true},
		"hardBreak": {attrs: []string{"text", "localId"}},
		"mention": {attrs: []string{
			"id", "localId", "text", "accessLevel", "userType",
		}, required: []string{"id"}},
		"emoji": {attrs: []string{"shortName", "id", "text", "localId"},
			required: []string{"shortName"}},
		"date": {attrs: []string{"timestamp", "localId"},
			required: []string{"timestamp"}},
		"status": {attrs: []string{"text", "color", "localId", "style"},
			required: []string{"text", "color"}},
		"placeholder": {attrs: []string{"text", "localId"},
			required: []string{"text"}},
		"inlineCard": {attrs: []string{"url", "data", "localId"}},
		"inlineExtension": {attrs: []string{
			"extensionKey", "extensionType", "parameters", "text", "localId",
		}, required: []string{"extensionKey", "extensionType"},
			marks: []string{"dataConsumer", "fragment"}},
		"mediaInline": {attrs: []string{
			"type", "localId", "id", "alt", "collection", "occurrenceKey",
			"width", "height", "data",
		}, required: []string{"id", "collection"}, marks: mediaMarks},
		"blockquote": {content: []string{
			"paragraph", "orderedList", "bulletList", "codeBlock",
			"mediaSingle", "mediaGroup", "extension",
		}, min: 1, attrs: []string{"localId"}},
		"bulletList": {content: []string{"listItem"}, min: 1,
			attrs: []string{"localId"}},
		"orderedList": {content: []string{"listItem"}, min: 1,
			attrs: []string{"order", "localId"}},
		"listItem": {content: []string{
			"paragraph", "bulletList", "orderedList", "taskList",
			"mediaSingle", "codeBlock", "extension",
		}, min: 1, attrs: []string{"localId"}},
		"codeBlock": {content: []string{"text"}, attrs: []string{
			"language", "uniqueId", "localId", "wrap", "hideLineNumbers",
		}},
		"rule": {attrs: []string{"localId"}},
		"panel": {content: []string{
			"paragraph", "heading", "bulletList", "orderedList", "blockCard",
			"mediaGroup", "mediaSingle", "codeBlock", "taskList", "rule",
			"decisionList", "extension",
		}, min: 1, attrs: []string{
			"panelType", "panelIcon", "panelIconId", "panelIconText",
			"panelColor", "localId",
		}, required: []string{"panelType"}},
		"table": {content: []string{"tableRow"}, min: 1, attrs: []string{
			"displayMode", "isNumberColumnEnabled", "layout", "localId",
			"width",
		}, marks: []string{"fragment"}},
		"tableRow": {content: []string{"tableCell", "tableHeader"},
			attrs: []string{"localId"}},
		"tableCell":   cellSpec,
		"tableHeader": cellSpec,
		"expand": {content: append(append([]string(nil), nonNestable...),
			"nestedExpand"), min: 1, attrs: []string{"title", "localId"}},
		"nestedExpand": {content: []string{
			"paragraph", "heading", "mediaSingle", "mediaGroup", "codeBlock",
			"bulletList", "orderedList", "taskList", "decisionList", "rule",
			"panel", "blockquote", "extension",
		}, min: 1, attrs: []string{"title", "localId"}},
		"bodiedExtension": {content: nonNestable, min: 1, attrs: extAttrs,
			required: []string{"extensionKey", "extensionType"},
			marks:    []string{"dataConsumer", "fragment"}},
		"extension": {attrs: extAttrs,
			required: []string{"extensionKey", "extensionType"},
			marks:    []string{"dataConsumer", "fragment"}},
		// The schema wants 2-3 columns, but Confluence stores one-column
		// sections too; accept what the product itself writes.
		"layoutSection": {content: []string{"layoutColumn"}, min: 1, max: 3,
			attrs: []string{"localId"}, marks: []string{"breakout"}},
		"layoutColumn": {content: []string{
			"blockCard", "paragraph", "mediaSingle", "codeBlock", "taskList",
			"bulletList", "orderedList", "heading", "mediaGroup",
			"decisionList", "rule", "panel", "blockquote", "extension",
			"embedCard", "table", "expand", "bodiedExtension",
		}, min: 1, attrs: []string{"width", "localId", "valign"},
			required: []string{"width"}},
		"taskList": {content: []string{"taskItem", "taskList", "blockTaskItem"},
			min: 1, attrs: []string{"localId"}, required: []string{"localId"}},
		"taskItem": {content: inlineTypes, attrs: []string{"localId", "state"},
			required: []string{"localId", "state"}},
		"blockTaskItem": {content: []string{"paragraph", "extension"}, min: 1,
			attrs:    []string{"localId", "state"},
			required: []string{"localId", "state"}},
		"decisionList": {content: []string{"decisionItem"}, min: 1,
			attrs: []string{"localId"}, required: []string{"localId"}},
		"decisionItem": {content: inlineTypes,
			attrs:    []string{"localId", "state"},
			required: []string{"localId", "state"}},
		"mediaSingle": {content: []string{"media", "caption"}, min: 1, max: 2,
			attrs: []string{"localId", "width", "layout", "widthType"},
			marks: []string{"link"}},
		"mediaGroup": {content: []string{"media"}, min: 1, noAttrs: true},
		"media": {attrs: []string{
			"type", "localId", "id", "alt", "collection", "height",
			"occurrenceKey", "width", "url",
		}, required: []string{"type"}, marks: mediaMarks},
		"caption": {content: []string{
			"hardBreak", "mention", "emoji", "date", "placeholder",
			"inlineCard", "status", "text",
		}, attrs: []string{"localId"}},
		"blockCard": {attrs: []string{
			"url", "datasource", "data", "localId", "width", "layout",
		}},
		"embedCard": {attrs: []string{
			"url", "layout", "width", "originalHeight", "originalWidth",
			"localId",
		}, required: []string{"url", "layout"}},
		"syncBlock": {attrs: []string{"resourceId", "localId"},
			required: []string{"resourceId", "localId"},
			marks:    []string{"breakout"}},
		"bodiedSyncBlock": {content: []string{
			"paragraph", "blockCard", "blockquote", "bulletList", "codeBlock",
			"decisionList", "embedCard", "expand", "heading", "layoutSection",
			"mediaGroup", "mediaSingle", "orderedList", "panel", "rule",
			"table", "taskList",
		}, min: 1, attrs: []string{"resourceId", "localId"},
			required: []string{"resourceId", "localId"},
			marks:    []string{"breakout"}},
	}
}}

var cellSpec = spec{content: []string{
	"paragraph", "panel", "blockquote", "orderedList", "bulletList", "rule",
	"heading", "codeBlock", "mediaSingle", "mediaGroup", "decisionList",
	"taskList", "blockCard", "embedCard", "extension", "nestedExpand",
}, min: 1, attrs: []string{
	"colspan", "rowspan", "colwidth", "background", "localId", "valign",
}}

// allowed reports whether a child of type child may appear in parent.
func allowed(parent, child string) bool {
	return slices.Contains(specs.get()[parent].content, child)
}

// nodeMarks lists the marks a non-text node may carry inside parent. The
// schema varies these by context, so they can't live in spec alone.
func nodeMarks(parent, child string) []string {
	root := parent == "doc"
	switch child {
	case "paragraph":
		switch parent {
		case "blockquote":
			return nil
		case "doc", "layoutColumn", "bodiedSyncBlock":
			return []string{"fontSize", "alignment", "indentation"}
		case "tableCell", "tableHeader":
			return []string{"fontSize", "alignment"}
		}
		return []string{"fontSize"}
	case "heading":
		switch parent {
		case "doc", "layoutColumn", "tableCell", "tableHeader",
			"bodiedSyncBlock":
			return []string{"alignment", "indentation"}
		}
		return nil
	case "rule", "panel":
		// Not in the schema, but Confluence writes it on wide pages.
		if root {
			return []string{"breakout"}
		}
		return nil
	case "codeBlock", "expand":
		if root {
			return []string{"breakout"}
		}
		return nil
	}
	if child != "text" && slices.Contains(inlineTypes, child) &&
		!slices.Contains(specs.get()[child].marks, "annotation") {
		// Inline comments anchor on any inline node: the schema file
		// allows annotation only on text and media, but Confluence puts it
		// on status, emoji and the rest, and edits must keep it.
		return append(slices.Clone(specs.get()[child].marks), "annotation")
	}
	return specs.get()[child].marks
}

// Mark sets for text: formatted text, or inline code (which combines
// only with link and annotation).
var (
	formattedMarks = []string{
		"link", "em", "strong", "strike", "subsup", "underline",
		"textColor", "annotation", "backgroundColor",
	}
	codeMarks = []string{"code", "link", "annotation"}
)

type markSpec struct {
	attrs    []string
	required []string
}

var markSpecs = lazy[map[string]markSpec]{build: func() map[string]markSpec {
	return map[string]markSpec{
		"code": {}, "em": {}, "strong": {}, "strike": {}, "underline": {},
		"link": {attrs: []string{
			"href", "title", "id", "collection", "occurrenceKey",
		}, required: []string{"href"}},
		"subsup":          {attrs: []string{"type"}, required: []string{"type"}},
		"textColor":       {attrs: []string{"color"}, required: []string{"color"}},
		"backgroundColor": {attrs: []string{"color"}, required: []string{"color"}},
		"alignment":       {attrs: []string{"align"}, required: []string{"align"}},
		"indentation":     {attrs: []string{"level"}, required: []string{"level"}},
		"breakout": {attrs: []string{"mode", "width"},
			required: []string{"mode"}},
		"fontSize": {attrs: []string{"fontSize"},
			required: []string{"fontSize"}},
		"annotation": {attrs: []string{"id", "annotationType"},
			required: []string{"id", "annotationType"}},
		"border": {attrs: []string{"size", "color"},
			required: []string{"size", "color"}},
		"dataConsumer": {attrs: []string{"sources"},
			required: []string{"sources"}},
		"fragment": {attrs: []string{"localId", "name"},
			required: []string{"localId"}},
	}
}}
