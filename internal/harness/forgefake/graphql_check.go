package forgefake

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The variable rules GitHub and GitLab (both graphql-ruby) validate a
// request against before running it, for the fake's GraphQL entry point
// and for tests that check the app's documents. A minimal tokenizer reads
// the operation's variable definitions and every $name the rest of the
// document uses; it skips strings, block strings and comments. It does
// not check a value against its declared type, nor that a field,
// argument or directive exists, and it refuses a document holding more
// than one definition (a second operation or a fragment), which the
// app's documents never do.

// GraphQLOperation is what ParseGraphQLOperation reads of a document.
type GraphQLOperation struct {
	// Kind is query, mutation or subscription; Name is empty for an
	// anonymous operation.
	Kind, Name string
	// Variables are the declared variables in order.
	Variables []GraphQLVariable
	// Uses are the variables the operation uses, each once, in order of
	// first use.
	Uses []string
}

// GraphQLVariable is one variable definition.
type GraphQLVariable struct {
	Name string
	// Type is the declared type as written, without spaces ("ID!",
	// "[String!]").
	Type       string
	NonNull    bool
	HasDefault bool
}

// GraphQLProblem is one validation error, with the forge's code.
type GraphQLProblem struct {
	Code     string
	Variable string
	Message  string
}

// GraphQLRequestProblems validates document and the variables sent with
// it: a variable used but not declared, declared but not used or
// declared twice, a non-null variable without a default that is missing
// or null, and a variable sent that the operation does not declare. A
// document it cannot read is one problem with code parseError.
func GraphQLRequestProblems(document string, vars map[string]any) []GraphQLProblem {
	op, err := ParseGraphQLOperation(document)
	if err != nil {
		return []GraphQLProblem{{Code: "parseError", Message: err.Error()}}
	}
	name := op.Name
	if name == "" {
		name = "anonymous " + op.Kind
	}
	var out []GraphQLProblem
	declared := map[string]GraphQLVariable{}
	for _, v := range op.Variables {
		if _, twice := declared[v.Name]; twice {
			out = append(out, GraphQLProblem{Code: "variableNotUnique", Variable: v.Name, Message: fmt.Sprintf("There can be only one variable named $%s", v.Name)})
			continue
		}
		declared[v.Name] = v
	}
	for _, use := range op.Uses {
		if _, ok := declared[use]; !ok {
			out = append(out, GraphQLProblem{Code: "variableNotDefined", Variable: use, Message: fmt.Sprintf("Variable $%s is used by %s but not declared", use, name)})
		}
	}
	for _, v := range op.Variables {
		if !slices.Contains(op.Uses, v.Name) {
			out = append(out, GraphQLProblem{Code: "variableNotUsed", Variable: v.Name, Message: fmt.Sprintf("Variable $%s is declared by %s but not used", v.Name, name)})
		}
		value, sent := vars[v.Name]
		if v.NonNull && !v.HasDefault && (!sent || value == nil) {
			out = append(out, GraphQLProblem{Code: "variableMismatch", Variable: v.Name, Message: fmt.Sprintf("Variable $%s of type %s was provided invalid value: Expected value to not be null", v.Name, v.Type)})
		}
	}
	sent := make([]string, 0, len(vars))
	for key := range vars {
		sent = append(sent, key)
	}
	slices.Sort(sent)
	for _, key := range sent {
		if _, ok := declared[key]; !ok {
			out = append(out, GraphQLProblem{Code: "variableNotDeclared", Variable: key, Message: fmt.Sprintf("Variable $%s is sent but not declared by %s", key, name)})
		}
	}
	return out
}

// ParseGraphQLOperation reads the one operation document defines.
func ParseGraphQLOperation(document string) (GraphQLOperation, error) {
	toks, err := graphQLTokens(document)
	if err != nil {
		return GraphQLOperation{}, err
	}
	var op GraphQLOperation
	i := 0
	at := func(text string) bool { return i < len(toks) && toks[i] == text }
	if at("{") {
		op.Kind = "query"
	} else {
		if !at("query") && !at("mutation") && !at("subscription") {
			return op, errors.New("the document does not start with an operation")
		}
		op.Kind = toks[i]
		i++
		if i < len(toks) && graphQLName(toks[i]) {
			op.Name = toks[i]
			i++
		}
		if at("(") {
			i++
			for !at(")") {
				if i >= len(toks) || !strings.HasPrefix(toks[i], "$") {
					return op, errors.New("a variable definition does not start with $name")
				}
				v := GraphQLVariable{Name: toks[i][1:]}
				i++
				if !at(":") {
					return op, fmt.Errorf("variable $%s has no type", v.Name)
				}
				i++
				for i < len(toks) && !at("=") && !at("@") && !at(")") && !strings.HasPrefix(toks[i], "$") {
					v.Type += toks[i]
					i++
				}
				if v.Type == "" {
					return op, fmt.Errorf("variable $%s has no type", v.Name)
				}
				v.NonNull = strings.HasSuffix(v.Type, "!")
				if at("=") {
					v.HasDefault = true
					i++
					if i, err = graphQLSkipValue(toks, i); err != nil {
						return op, err
					}
				}
				for at("@") {
					if i, err = graphQLSkipDirective(toks, i); err != nil {
						return op, err
					}
				}
				op.Variables = append(op.Variables, v)
			}
			i++
		}
	}
	// The directives and selection set: every $name in them is a use.
	depth := 0
	for ; i < len(toks); i++ {
		switch tok := toks[i]; {
		case tok == "{":
			depth++
		case tok == "}":
			depth--
			if depth < 0 {
				return op, errors.New("unbalanced braces")
			}
			if depth == 0 {
				if i+1 < len(toks) {
					return op, errors.New("the document holds more than one definition")
				}
				return op, nil
			}
		case strings.HasPrefix(tok, "$"):
			if !slices.Contains(op.Uses, tok[1:]) {
				op.Uses = append(op.Uses, tok[1:])
			}
		}
	}
	return op, errors.New("the operation has no closed selection set")
}

// graphQLSkipValue skips one value from toks[i]: a list or object with
// what it nests, or a single token.
func graphQLSkipValue(toks []string, i int) (int, error) {
	depth := 0
	for ; i < len(toks); i++ {
		switch toks[i] {
		case "[", "{":
			depth++
		case "]", "}":
			depth--
		}
		if depth == 0 {
			return i + 1, nil
		}
	}
	return i, errors.New("a default value is not closed")
}

// graphQLSkipDirective skips @name and its argument list from toks[i].
func graphQLSkipDirective(toks []string, i int) (int, error) {
	i += 2
	if i < len(toks) && toks[i] == "(" {
		depth := 0
		for ; i < len(toks); i++ {
			switch toks[i] {
			case "(":
				depth++
			case ")":
				depth--
			}
			if depth == 0 {
				return i + 1, nil
			}
		}
		return i, errors.New("a directive's arguments are not closed")
	}
	return i, nil
}

func graphQLName(tok string) bool {
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

// graphQLTokens splits document into punctuators, names, $variables and
// numbers. Strings, block strings, comments, commas and white space are
// dropped.
func graphQLTokens(document string) ([]string, error) {
	var toks []string
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
			toks = append(toks, string(c))
			i++
		case c == '$' || c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
			j := i + 1
			for j < len(s) && (s[j] == '_' || ('a' <= s[j] && s[j] <= 'z') || ('A' <= s[j] && s[j] <= 'Z') || ('0' <= s[j] && s[j] <= '9')) {
				j++
			}
			tok := s[i:j]
			if c == '$' && !graphQLName(tok[1:]) {
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
