package usermessage

import (
	"strings"
	"unicode"
)

// Composer slash commands (D31). A whitespace-delimited word of the form
// `/name` — anywhere in the message, not only at the front — invokes the
// command called `name`: the provider receives the typed text with the
// command's block appended, the stored row keeps the typed text, and
// Meta.Command records which command expanded.
//
// This file owns the parse — what counts as a command word — because the same
// rule has to hold on both sides of the split: the send path asks "did the
// user invoke a command", and history rendering asks "where in this row's text
// does the command its meta names appear". The TABLE of commands and what each
// one expands to lives in `internal/app`, where the resolvers can reach the
// store. The frontend's own copy of the rule is
// `frontend/src/lib/utils/commandWords.ts` (with the registry in
// `frontend/src/lib/components/composer/slashCommands.ts`) — the two are
// deliberately parallel, and the backend is authoritative: expansion happens
// here, and the composer's colouring is an affordance for it.

// CommandWords returns every command-shaped word in content, in order of
// appearance and without its leading slash. Rules, mirrored exactly by the
// frontend matcher:
//
//   - a word is a maximal run of non-whitespace, so a candidate `/` must sit
//     at the start of the content or immediately after whitespace and the name
//     runs to the next whitespace character (or the end);
//   - the name must be lowercase `[a-z][a-z0-9-]*`, so `/tmp/foo`, `/Users`,
//     and a bare `/` are ordinary text.
//
// Whether a returned name is REGISTERED is the caller's question; this
// function only reports shapes, in the order the caller should consider them.
// Duplicates are preserved — a message that says `/workflow` twice yields it
// twice — because the caller decides what repetition means.
//
// `unicode.IsSpace` is exactly the Unicode White_Space property, which is what
// the frontend's separator test is written to match; the two never disagree
// about where a word ends.
func CommandWords(content string) []string {
	var words []string
	for _, word := range strings.FieldsFunc(content, unicode.IsSpace) {
		name, ok := strings.CutPrefix(word, "/")
		if !ok || !validCommandWord(name) {
			continue
		}
		words = append(words, name)
	}
	return words
}

// validCommandWord reports whether word is a syntactically valid command name.
func validCommandWord(word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '-':
		default:
			return false
		}
	}
	return true
}
