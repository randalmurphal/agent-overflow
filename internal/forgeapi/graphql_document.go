package forgeapi

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Document is a GraphQL document a Client sends: its forge, the one
// operation it defines and its text. NewDocument is the only way to make
// one and registers each, so every document a binary can send is known
// (Documents) without reading the source that sends it; tests validate
// them against the forges' schemas. Build documents once, as
// package-level values.
type Document struct {
	forge, operation, text string
}

var (
	documentsMu sync.Mutex
	documents   []Document
)

// NewDocument makes and registers the document text for forge
// (ForgeGitHub or ForgeGitLab). Its operation is the one named operation
// text defines, beside any fragments. It panics on another forge, on
// text without exactly one named operation, and on a second document of
// the forge that names the same operation: the fake forge and the forges'
// logs tell requests apart by it, and a document is built once.
func NewDocument(forge, text string) Document {
	if forge != ForgeGitHub && forge != ForgeGitLab {
		panic(fmt.Sprintf("forgeapi: a GraphQL document for forge %q", forge))
	}
	operation, err := documentOperation(text)
	if err != nil {
		panic(fmt.Sprintf("forgeapi: GraphQL document: %v:\n%s", err, text))
	}
	d := Document{forge: forge, operation: operation, text: text}
	documentsMu.Lock()
	defer documentsMu.Unlock()
	if slices.ContainsFunc(documents, func(o Document) bool { return o.forge == forge && o.operation == operation }) {
		panic(fmt.Sprintf("forgeapi: two %s GraphQL documents define operation %s", forge, operation))
	}
	documents = append(documents, d)
	return d
}

// Documents are every document NewDocument made, in the order made.
func Documents() []Document {
	documentsMu.Lock()
	defer documentsMu.Unlock()
	return slices.Clone(documents)
}

// Forge is ForgeGitHub or ForgeGitLab.
func (d Document) Forge() string { return d.forge }

// Operation is the name of the operation the document defines, sent as
// operationName.
func (d Document) Operation() string { return d.operation }

// Text is the document.
func (d Document) Text() string { return d.text }

// documentOperation is the name of the one named operation text defines.
// Fragment definitions beside it are skipped.
func documentOperation(text string) (string, error) {
	toks, err := GraphQLTokens(text)
	if err != nil {
		return "", err
	}
	var names []string
	for i := 0; i < len(toks); {
		switch toks[i] {
		case "query", "mutation", "subscription":
			name := ""
			if i+1 < len(toks) && GraphQLName(toks[i+1]) {
				name = toks[i+1]
			}
			names = append(names, name)
		case "fragment", "{":
		default:
			return "", fmt.Errorf("a definition starts with %q", toks[i])
		}
		if toks[i] == "{" {
			names = append(names, "")
		}
		// Skip to the definition's selection set, past any parenthesized
		// variables (whose defaults may hold braces), then past it.
		parens := 0
		for ; i < len(toks) && (toks[i] != "{" || parens > 0); i++ {
			switch toks[i] {
			case "(":
				parens++
			case ")":
				parens--
			}
		}
		depth := 0
		for ; i < len(toks); i++ {
			if toks[i] == "{" {
				depth++
			} else if toks[i] == "}" {
				if depth--; depth == 0 {
					i++
					break
				}
			}
		}
		if depth != 0 {
			return "", errors.New("a selection set is not closed")
		}
	}
	switch {
	case len(names) != 1:
		return "", fmt.Errorf("it defines %d operations, not one", len(names))
	case names[0] == "":
		return "", errors.New("its operation has no name")
	}
	return names[0], nil
}

// GraphQLName reports whether tok is a GraphQL name.
func GraphQLName(tok string) bool {
	if tok == "" {
		return false
	}
	for j, r := range tok {
		letter := r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
		digit := j > 0 && '0' <= r && r <= '9'
		if !letter && !digit {
			return false
		}
	}
	return true
}

// GraphQLTokens splits document into punctuators, names, $variables and
// numbers. Strings, block strings, comments, commas and white space are
// dropped.
func GraphQLTokens(document string) ([]string, error) {
	toks := make([]string, 0, len(document)/4)
	s := document
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case strings.HasPrefix(s[i:], "\ufeff"):
			i += len("\ufeff")
		case c == '#':
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case strings.HasPrefix(s[i:], `"""`):
			i += 3
			for {
				if i >= len(s) {
					return nil, errors.New("a block string is not closed")
				}
				if strings.HasPrefix(s[i:], `\"""`) {
					i += 4
					continue
				}
				if strings.HasPrefix(s[i:], `"""`) {
					i += 3
					break
				}
				i++
			}
		case c == '"':
			i++
			for {
				if i >= len(s) || s[i] == '\n' || s[i] == '\r' {
					return nil, errors.New("a string is not closed")
				}
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					break
				}
				i++
			}
		case strings.HasPrefix(s[i:], "..."):
			toks = append(toks, "...")
			i += 3
		case strings.IndexByte("!():=@[]{}|&", c) >= 0:
			toks = append(toks, s[i:i+1])
			i++
		case c == '$' || c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
			j := i + 1
			for j < len(s) && (s[j] == '_' || ('a' <= s[j] && s[j] <= 'z') || ('A' <= s[j] && s[j] <= 'Z') || ('0' <= s[j] && s[j] <= '9')) {
				j++
			}
			tok := s[i:j]
			if c == '$' && !GraphQLName(tok[1:]) {
				return nil, fmt.Errorf("%q is not a variable", tok)
			}
			toks = append(toks, tok)
			i = j
		case c == '-' || ('0' <= c && c <= '9'):
			j := i + 1
			for j < len(s) && strings.IndexByte("0123456789.eE+-", s[j]) >= 0 {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return toks, nil
}
