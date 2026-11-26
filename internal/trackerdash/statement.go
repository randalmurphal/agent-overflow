package trackerdash

import (
	"errors"
	"strings"
)

// oneStatement is sql as one statement the wrapper can hold between its
// parentheses: outside strings, quoted identifiers and comments, its
// parentheses balance and never close more than they opened, and it has no
// semicolon but one at its end, which is cut. So the wrapper's closing
// parenthesis, which follows on a line of its own, always closes it, and
// nothing after the statement runs.
func oneStatement(sql string) (string, error) {
	depth := 0
	end := -1 // the index of a semicolon that ends the statement
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if end >= 0 && !isSpace(c) && !startsComment(sql, i) {
			return "", errors.New("holds more than one statement")
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := closeQuoted(sql, i+1, c)
			if j < 0 {
				return "", errors.New("has a string or a quoted name that is never closed")
			}
			i = j
		case c == '[':
			j := strings.IndexByte(sql[i+1:], ']')
			if j < 0 {
				return "", errors.New("has a quoted name that is never closed")
			}
			i += j + 1
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
				return "", errors.New("has a comment that is never closed")
			}
			i += j + 3
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return "", errors.New("closes a parenthesis it never opened")
			}
		case c == ';':
			if depth != 0 {
				return "", errors.New("has a semicolon inside parentheses")
			}
			end = i
		}
	}
	if depth != 0 {
		return "", errors.New("leaves a parenthesis open")
	}
	if end >= 0 {
		sql = sql[:end]
	}
	if strings.TrimSpace(sql) == "" {
		return "", errors.New("is empty")
	}
	return sql, nil
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
