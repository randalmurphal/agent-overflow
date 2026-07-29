package usermessage

import (
	"strings"
	"unicode"
)

// Composer slash commands (D31). A message whose FIRST whitespace-delimited
// word is `/name` invokes the command called `name`: the provider receives the
// typed text with the command's block appended, the stored row keeps the typed
// text, and Meta.Command records which command expanded.
//
// This file owns the parse — what counts as a leading command word — because
// the same rule has to hold on both sides of the split: the send path asks
// "did the user invoke a command", and history rendering asks "does this row's
// leading word match the command its meta names". The TABLE of commands and
// what each one expands to lives in the main package, where the resolvers can
// reach the store. The frontend's own copy of the rule (and of the table) is
// `frontend/src/lib/components/composer/slashCommands.ts` — the two are
// deliberately parallel, and the backend is authoritative: expansion happens
// here, and the composer's colouring is an affordance for it.

// LeadingCommandWord returns the command name (without the leading slash) when
// content opens with one, and "" otherwise. Rules, mirrored exactly by the
// frontend matcher:
//
//   - the very first character must be `/` — no leading whitespace, because a
//     message that begins with a space is not a command invocation and the
//     composer does not colour it as one;
//   - the name runs to the first whitespace character (or end of content);
//   - the name must be lowercase `[a-z][a-z0-9-]*`, so prose openers like
//     `/tmp/foo`, `/Users`, or a bare `/` never read as a command.
//
// Whether the returned name is REGISTERED is the caller's question; this
// function only reports the shape.
func LeadingCommandWord(content string) string {
	if !strings.HasPrefix(content, "/") {
		return ""
	}
	word := content[1:]
	if idx := strings.IndexFunc(word, unicode.IsSpace); idx >= 0 {
		word = word[:idx]
	}
	if !validCommandWord(word) {
		return ""
	}
	return word
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
