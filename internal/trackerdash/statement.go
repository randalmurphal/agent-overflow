package trackerdash

import (
	"errors"
	"strings"
)

// words is what a statement's text names: each bare identifier and the
// text of each quoted run ("..", `..`, [..] and '..', since SQLite reads a
// single-quoted string as a name where only a name fits), ASCII
// lower-cased as SQLite compares names, and whether a * stands outside
// strings, quoted names and comments. A statement on the board's copy
// reads a relation only by naming it, and a column only by naming it, by
// a * or by a NATURAL join: the copy has no views or triggers and its
// catalog is denied. So what words holds is all a statement can read,
// and more where a name is an alias, a string or a CTE's.
type words struct {
	names map[string]bool
	star  bool
}

// named reports whether the text names name, a lower-case name.
func (w words) named(name string) bool { return w.names[name] }

// oneStatement is sql as one statement the wrapper can hold between its
// parentheses: outside strings, quoted identifiers and comments, its
// parentheses balance and never close more than they opened, and it has no
// semicolon but one at its end, which is cut. So the wrapper's closing
// parenthesis, which follows on a line of its own, always closes it, and
// nothing after the statement runs. It also answers the statement's words.
func oneStatement(sql string) (string, words, error) {
	w := words{names: map[string]bool{}}
	name := func(n string) { w.names[strings.ToLower(n)] = true }
	depth := 0
	end := -1 // the index of a semicolon that ends the statement
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if end >= 0 && !isSpace(c) && !startsComment(sql, i) {
			return "", words{}, errors.New("holds more than one statement")
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := closeQuoted(sql, i+1, c)
			if j < 0 {
				return "", words{}, errors.New("has a string or a quoted name that is never closed")
			}
			name(strings.ReplaceAll(sql[i+1:j], string([]byte{c, c}), string(c)))
			i = j
		case c == '[':
			j := strings.IndexByte(sql[i+1:], ']')
			if j < 0 {
				return "", words{}, errors.New("has a quoted name that is never closed")
			}
			name(sql[i+1 : i+1+j])
			i += j + 1
		case isIdent(c):
			j := i
			for j < len(sql) && isIdent(sql[j]) {
				j++
			}
			name(sql[i:j])
			i = j - 1
		case c == '*':
			w.star = true
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				i = len(sql)
			} else {
				i += j
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				return "", words{}, errors.New("has a comment that is never closed")
			}
			i += j + 3
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return "", words{}, errors.New("closes a parenthesis it never opened")
			}
		case c == ';':
			if depth != 0 {
				return "", words{}, errors.New("has a semicolon inside parentheses")
			}
			end = i
		}
	}
	if depth != 0 {
		return "", words{}, errors.New("leaves a parenthesis open")
	}
	if end >= 0 {
		sql = sql[:end]
	}
	if strings.TrimSpace(sql) == "" {
		return "", words{}, errors.New("is empty")
	}
	return sql, w, nil
}

// isIdent is whether c is a byte of a bare name, as SQLite's tokenizer
// has it: a letter, a digit, _, $ or any byte of a non-ASCII character.
// Every other byte ends a name, so a qualified name is its parts.
func isIdent(c byte) bool {
	return c >= 0x80 || c == '_' || c == '$' || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

// closeQuoted is the index of the quote q that closes a quoted run
// starting at i, a doubled quote being one inside it; -1 when none does.
func closeQuoted(sql string, i int, q byte) int {
	for ; i < len(sql); i++ {
		if sql[i] != q {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == q {
			i++
			continue
		}
		return i
	}
	return -1
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// startsComment is whether a comment starts at sql[i].
func startsComment(sql string, i int) bool {
	return i+1 < len(sql) && ((sql[i] == '-' && sql[i+1] == '-') || (sql[i] == '/' && sql[i+1] == '*'))
}
