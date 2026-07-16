package highlight

import "strings"

// Class ids are the wire contract with the frontend: spans carry a
// class id, the frontend maps id → `syntax-<name>` CSS class via the
// HighlightClassNames binding, and app.css owns the colors per theme.
//
// The taxonomy is a deliberately small coalesced family set (~2 dozen
// classes), not the full tree-sitter capture space: a theme colors
// families, not individual captures. Changing an entry here is a
// coordinated Go + app.css change — treat the list as append-only.
const (
	ClassNone uint16 = iota
	ClassKeyword
	ClassString
	ClassStringSpecial // escapes, regexes, interpolation delimiters, urls
	ClassComment
	ClassNumber
	ClassFunction
	ClassType
	ClassVariable
	ClassVariableBuiltin // self, this, super
	ClassParameter
	ClassProperty
	ClassConstant
	ClassOperator
	ClassPunctuation
	ClassTag
	ClassAttribute
	ClassNamespace
	ClassLabel
	ClassEmbedded
	ClassMarkupHeading
	ClassMarkupBold
	ClassMarkupItalic
	ClassMarkupLink
	ClassMarkupRaw
	ClassMarkupList
	ClassMarkupQuote
	ClassAdded
	ClassRemoved

	classCount // keep last
)

// classNames is indexed by class id; index 0 ("none") means unstyled.
// These names ARE the CSS contract: frontend renders `syntax-<name>`.
var classNames = [classCount]string{
	ClassNone:            "none",
	ClassKeyword:         "keyword",
	ClassString:          "string",
	ClassStringSpecial:   "string-special",
	ClassComment:         "comment",
	ClassNumber:          "number",
	ClassFunction:        "function",
	ClassType:            "type",
	ClassVariable:        "variable",
	ClassVariableBuiltin: "variable-builtin",
	ClassParameter:       "parameter",
	ClassProperty:        "property",
	ClassConstant:        "constant",
	ClassOperator:        "operator",
	ClassPunctuation:     "punctuation",
	ClassTag:             "tag",
	ClassAttribute:       "attribute",
	ClassNamespace:       "namespace",
	ClassLabel:           "label",
	ClassEmbedded:        "embedded",
	ClassMarkupHeading:   "markup-heading",
	ClassMarkupBold:      "markup-bold",
	ClassMarkupItalic:    "markup-italic",
	ClassMarkupLink:      "markup-link",
	ClassMarkupRaw:       "markup-raw",
	ClassMarkupList:      "markup-list",
	ClassMarkupQuote:     "markup-quote",
	ClassAdded:           "added",
	ClassRemoved:         "removed",
}

// ClassNames returns the id → name table (index = class id). The
// slice is freshly allocated; callers own it.
func ClassNames() []string {
	out := make([]string, classCount)
	copy(out, classNames[:])
	return out
}

// captureFamilyExact holds capture names that need a verdict a prefix
// rule can't give (checked before the prefix walk).
var captureFamilyExact = map[string]uint16{
	"constructor":        ClassFunction,
	"method":             ClassFunction,
	"field":              ClassProperty,
	"self":               ClassVariableBuiltin,
	"parameter":          ClassParameter,
	"module":             ClassNamespace,
	"escape":             ClassStringSpecial,
	"special":            ClassStringSpecial, // helix scope: ? operator, derive, other special symbols
	"conceal":            ClassNone,
	"spell":              ClassNone,
	"nospell":            ClassNone,
	"none":               ClassNone,
	"error":              ClassNone,
	"injection.content":  ClassNone,
	"injection.language": ClassNone,
	// nvim legacy names still present in some vendored queries.
	"include":        ClassKeyword,
	"conditional":    ClassKeyword,
	"repeat":         ClassKeyword,
	"exception":      ClassKeyword,
	"storageclass":   ClassKeyword,
	"define":         ClassKeyword,
	"preproc":        ClassKeyword,
	"float":          ClassNumber,
	"boolean":        ClassConstant,
	"character":      ClassString,
	"symbol":         ClassConstant,
	"text.title":     ClassMarkupHeading,
	"text.literal":   ClassMarkupRaw,
	"text.uri":       ClassMarkupLink,
	"text.strong":    ClassMarkupBold,
	"text.emphasis":  ClassMarkupItalic,
	"text.quote":     ClassMarkupQuote,
	"text.reference": ClassMarkupLink,
	"diff.plus":      ClassAdded,
	"diff.minus":     ClassRemoved,
	"diff.delta":     ClassStringSpecial,
}

// captureFamilyPrefixes maps capture-name prefixes to class families,
// most-specific first. A capture like `keyword.control.import` or
// `string.special.url` resolves by the longest matching prefix.
var captureFamilyPrefixes = []struct {
	prefix string
	class  uint16
}{
	{"comment", ClassComment},
	{"string.special", ClassStringSpecial},
	{"string.escape", ClassStringSpecial},
	{"string.regexp", ClassStringSpecial},
	{"string.regex", ClassStringSpecial},
	{"string", ClassString},
	{"character.special", ClassStringSpecial},
	{"number", ClassNumber},
	{"keyword", ClassKeyword},
	{"function", ClassFunction},
	{"method", ClassFunction},
	{"constructor", ClassFunction},
	{"type", ClassType},
	{"class", ClassType},
	{"struct", ClassType},
	{"enum", ClassType},
	{"interface", ClassType},
	{"variable.builtin", ClassVariableBuiltin},
	{"variable.parameter", ClassParameter},
	{"variable.member", ClassProperty},
	{"variable.other.member", ClassProperty},
	{"variable", ClassVariable},
	{"property", ClassProperty},
	// Helix numeric/character conventions — must outrank "constant".
	{"constant.numeric", ClassNumber},
	{"constant.character.escape", ClassStringSpecial},
	{"constant.character", ClassString},
	{"constant", ClassConstant},
	{"operator", ClassOperator},
	{"punctuation", ClassPunctuation},
	{"tag.attribute", ClassAttribute},
	{"tag.delimiter", ClassPunctuation},
	{"tag", ClassTag},
	{"attribute", ClassAttribute},
	{"namespace", ClassNamespace},
	{"module", ClassNamespace},
	{"label", ClassLabel},
	{"embedded", ClassEmbedded},
	{"markup.heading", ClassMarkupHeading},
	{"markup.bold", ClassMarkupBold},
	{"markup.strong", ClassMarkupBold},
	{"markup.italic", ClassMarkupItalic},
	{"markup.strikethrough", ClassMarkupItalic},
	{"markup.link", ClassMarkupLink},
	{"markup.url", ClassMarkupLink},
	{"markup.raw", ClassMarkupRaw},
	{"markup.list", ClassMarkupList},
	{"markup.quote", ClassMarkupQuote},
	{"text", ClassNone},
	{"diff.addition", ClassAdded},
	{"diff.deletion", ClassRemoved},
}

// captureFamily resolves a tree-sitter capture name (without the `@`)
// to a class id. ok=false means the capture is unknown to the
// taxonomy — the query-compile harness fails CI on those so vendored
// queries can't silently lose captures; at runtime they render plain.
func captureFamily(name string) (uint16, bool) {
	if class, found := captureFamilyExact[name]; found {
		return class, true
	}
	for _, entry := range captureFamilyPrefixes {
		if name == entry.prefix || strings.HasPrefix(name, entry.prefix+".") {
			return entry.class, true
		}
	}
	// Private captures (leading underscore) are query-internal helper
	// captures by convention, never styled.
	if strings.HasPrefix(name, "_") {
		return ClassNone, true
	}
	return ClassNone, false
}
