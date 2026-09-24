package store

import "strings"

// sqlToken is a token class of SQLite's sqlite3_complete (complete.c).
type sqlToken uint8

const (
	tkSemi sqlToken = iota
	tkWS
	tkOther
	tkExplain
	tkCreate
	tkTemp
	tkTrigger
	tkEnd
)

// sqlCompleteTrans is sqlite3_complete's state machine. States: 0 invalid,
// 1 start, 2 normal, 3 explain, 4 create, 5 trigger body, 6 semicolon in a
// trigger body, 7 end of a trigger body. A semicolon that reaches state 1
// ends a statement.
var sqlCompleteTrans = [8][8]uint8{
	//  SEMI WS OTHER EXPLAIN CREATE TEMP TRIGGER END
	{1, 0, 2, 3, 4, 2, 2, 2},
	{1, 1, 2, 3, 4, 2, 2, 2},
	{1, 2, 2, 2, 2, 2, 2, 2},
	{1, 3, 3, 2, 4, 2, 2, 2},
	{1, 4, 2, 2, 2, 4, 5, 2},
	{6, 5, 5, 5, 5, 5, 5, 5},
	{6, 6, 5, 5, 5, 5, 5, 7},
	{1, 7, 5, 5, 5, 5, 5, 5},
}

// sqlStatements splits a script into its statements, each ending with
// its semicolon (sqlStatementEnds). Statements holding only whitespace,
// comments and semicolons are dropped. Trailing text without a closing
// semicolon is returned as a final statement for SQLite to judge.
func sqlStatements(script string) []string {
	var out []string
	start := 0
	for _, end := range append(sqlStatementEnds(script), len(script)) {
		if stmt := script[start:end]; sqlHasToken(stmt) {
			out = append(out, stmt)
		}
		start = end
	}
	return out
}

// sqlStatementEnds is the offset just past each semicolon that ends a
// statement, where SQLite's sqlite3_complete finds one: a semicolon
// outside strings, quoted identifiers, comments and CREATE TRIGGER
// bodies. An empty statement's semicolon counts.
func sqlStatementEnds(script string) []int {
	var ends []int
	state := uint8(0)
	for i := 0; i < len(script); {
		kind, next := scanSQLToken(script, i)
		state = sqlCompleteTrans[state][kind]
		i = next
		if kind == tkSemi && state == 1 {
			ends = append(ends, i)
		}
	}
	return ends
}

// sqlHasToken reports whether s holds anything but whitespace, comments
// and semicolons.
func sqlHasToken(s string) bool {
	for i := 0; i < len(s); {
		kind, next := scanSQLToken(s, i)
		if kind != tkWS && kind != tkSemi {
			return true
		}
		i = next
	}
	return false
}

// scanSQLToken reads the token at s[i], returning its class and the index
// after it. A string, quoted identifier or comment that is not closed runs
// to the end of s, so no later semicolon ends a statement.
func scanSQLToken(s string, i int) (kind sqlToken, next int) {
	switch c := s[i]; c {
	case ';':
		return tkSemi, i + 1
	case ' ', '\r', '\t', '\n', '\f':
		return tkWS, i + 1
	case '/':
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return tkWS, len(s)
			}
			return tkWS, i + 2 + end + 2
		}
	case '-':
		if strings.HasPrefix(s[i:], "--") {
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return tkWS, len(s)
			}
			return tkWS, i + end + 1
		}
	case '[':
		end := strings.IndexByte(s[i+1:], ']')
		if end < 0 {
			return tkOther, len(s)
		}
		return tkOther, i + 1 + end + 1
	case '`', '"', '\'':
		end := strings.IndexByte(s[i+1:], c)
		if end < 0 {
			return tkOther, len(s)
		}
		return tkOther, i + 1 + end + 1
	}
	if !isSQLIdentChar(s[i]) {
		return tkOther, i + 1
	}
	j := i + 1
	for j < len(s) && isSQLIdentChar(s[j]) {
		j++
	}
	switch word := s[i:j]; {
	case strings.EqualFold(word, "create"):
		return tkCreate, j
	case strings.EqualFold(word, "trigger"):
		return tkTrigger, j
	case strings.EqualFold(word, "temp"), strings.EqualFold(word, "temporary"):
		return tkTemp, j
	case strings.EqualFold(word, "end"):
		return tkEnd, j
	case strings.EqualFold(word, "explain"):
		return tkExplain, j
	}
	return tkOther, j
}

// isSQLIdentChar matches SQLite's IdChar: letters, digits, '_', '$' and
// every byte of a multi-byte UTF-8 character.
func isSQLIdentChar(c byte) bool {
	return c >= 0x80 || c == '_' || c == '$' ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// createIndexName is the index a CREATE [UNIQUE] INDEX [IF NOT EXISTS]
// statement builds, as written.
func createIndexName(stmt string) (string, bool) {
	var words []string
	for i := 0; i < len(stmt) && len(words) < 9; {
		kind, next := scanSQLToken(stmt, i)
		if kind != tkWS {
			words = append(words, stmt[i:next])
		}
		i = next
	}
	k := 0
	take := func(word string) bool {
		if k < len(words) && strings.EqualFold(words[k], word) {
			k++
			return true
		}
		return false
	}
	if !take("create") {
		return "", false
	}
	take("unique")
	if !take("index") {
		return "", false
	}
	if take("if") && !(take("not") && take("exists")) {
		return "", false
	}
	if k >= len(words) {
		return "", false
	}
	name := words[k]
	if k+2 < len(words) && words[k+1] == "." {
		name += "." + words[k+2]
	}
	return name, true
}
