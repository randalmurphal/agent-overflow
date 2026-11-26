package trackerfake

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The JQL the fake reads is the forms the clients write, parsed for real
// so a clause in another form, field or operator is refused by name:
//
//	query   = [or] ["ORDER BY" key {"," key}]
//	or      = and {"OR" and}
//	and     = unary {"AND" unary}
//	unary   = "(" or ")" | clause
//	clause  = updated (> | >=) <ms>
//	        | id (> | !=) <n> | id in (<n>, ...)
//	        | key in ("<key>", ...)
//	        | statusCategory (= | !=) <category>
//	        | sprint in (<n>, ...) | sprint in openSprints()
//	        | assignee = currentUser()
//	        | status = "<name>"
//	        | text ~ "<text search>"
//	        | project = "<key or ID>"
//	        | filter = <n>
//	        | status in (<n>, ...)
//	        | hierarchyLevel (= | !=) <n>
//	        | worklogAuthor = "<account ID>"
//	        | worklogDate (>= | <=) "<YYYY-MM-DD>"
//	key     = (updated | id | created | key) [ASC | DESC]
//
// Measured: an epoch-millisecond literal compares at millisecond
// precision with no time zone, `ORDER BY updated ASC, id ASC` sorts at
// that precision, and the board lists read the same JQL. Measured on a
// site: openSprints() holds the started sprints only (247 issues, against
// 3,390 in futureSprints() and none in both).

// jqlQuery is a parsed query: a filter and an order. An empty order is
// the search's default.
type jqlQuery struct {
	where func(s *jiraSite, is *JiraIssue) bool
	order []jqlOrder
	// sprints are the sprint IDs a sprint in clause names, filters the
	// saved filter IDs a filter clause names, and statuses the status
	// names a status clause names.
	sprints  []int64
	filters  []string
	statuses []string
}

type jqlOrder struct {
	field string
	desc  bool
}

// jqlError is a JQL form the fake does not implement.
type jqlError struct{ msg string }

func (e *jqlError) Error() string { return "JQL " + e.msg }

type jqlToken struct {
	kind byte // w word, s string, n number, o operator, p punctuation, 0 end
	text string
}

func lexJQL(src string) ([]jqlToken, error) {
	var out []jqlToken
	rs := []rune(src)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(' || r == ')' || r == ',':
			out = append(out, jqlToken{'p', string(r)})
			i++
		case r == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] != '\\' {
					b.WriteRune(rs[j])
					continue
				}
				if j++; j == len(rs) {
					return nil, &jqlError{"string ends in a backslash"}
				}
				switch rs[j] {
				case 'n':
					b.WriteRune('\n')
				case 'r':
					b.WriteRune('\r')
				case 't':
					b.WriteRune('\t')
				default:
					b.WriteRune(rs[j])
				}
			}
			if j == len(rs) {
				return nil, &jqlError{"string is not closed"}
			}
			out = append(out, jqlToken{'s', b.String()})
			i = j + 1
		case strings.ContainsRune("<>=!~", r):
			j := i + 1
			if j < len(rs) && rs[j] == '=' {
				j++
			}
			out = append(out, jqlToken{'o', string(rs[i:j])})
			i = j
		default:
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_' || rs[j] == '-' || rs[j] == '.') {
				j++
			}
			if j == i {
				return nil, &jqlError{fmt.Sprintf("character %q is not implemented", r)}
			}
			word := string(rs[i:j])
			kind := byte('w')
			if _, err := strconv.ParseInt(word, 10, 64); err == nil {
				kind = 'n'
			}
			out = append(out, jqlToken{kind, word})
			i = j
		}
	}
	return append(out, jqlToken{}), nil
}

type jqlParser struct {
	toks []jqlToken
	pos  int
	// viewer is the account currentUser() names, empty where no one
	// signed in asks (a board's sub-query, read when seeded).
	viewer   string
	sprints  []int64
	filters  []string
	statuses []string
}

func (p *jqlParser) peek() jqlToken { return p.toks[p.pos] }
func (p *jqlParser) next() jqlToken {
	t := p.toks[p.pos]
	if t.kind != 0 {
		p.pos++
	}
	return t
}

// keyword reports and consumes the word w, case ignored.
func (p *jqlParser) keyword(w string) bool {
	if t := p.peek(); t.kind == 'w' && strings.EqualFold(t.text, w) {
		p.pos++
		return true
	}
	return false
}

// parseJQL reads src in the forms the fake implements, as viewer asks it.
func parseJQL(src, viewer string) (jqlQuery, error) {
	toks, err := lexJQL(src)
	if err != nil {
		return jqlQuery{}, err
	}
	p := &jqlParser{toks: toks, viewer: viewer}
	q := jqlQuery{where: func(*jiraSite, *JiraIssue) bool { return true }}
	t := p.peek()
	if order := t.kind == 'w' && strings.EqualFold(t.text, "ORDER"); t.kind != 0 && !order {
		if q.where, err = p.or(); err != nil {
			return jqlQuery{}, err
		}
	}
	if p.keyword("ORDER") {
		if !p.keyword("BY") {
			return jqlQuery{}, &jqlError{"ORDER is not followed by BY"}
		}
		for {
			t := p.next()
			field := strings.ToLower(t.text)
			if t.kind != 'w' || !slices.Contains([]string{"updated", "id", "created", "key"}, field) {
				return jqlQuery{}, &jqlError{fmt.Sprintf("ORDER BY %q is not implemented", t.text)}
			}
			o := jqlOrder{field: field}
			if p.keyword("DESC") {
				o.desc = true
			} else {
				p.keyword("ASC")
			}
			q.order = append(q.order, o)
			if t := p.peek(); t.kind != 'p' || t.text != "," {
				break
			}
			p.next()
		}
	}
	if t := p.peek(); t.kind != 0 {
		return jqlQuery{}, &jqlError{fmt.Sprintf("text from %q on is not implemented", t.text)}
	}
	q.sprints, q.filters, q.statuses = p.sprints, p.filters, p.statuses
	return q, nil
}

type jqlFilter = func(s *jiraSite, is *JiraIssue) bool

func (p *jqlParser) or() (jqlFilter, error) {
	left, err := p.and()
	for err == nil && p.keyword("OR") {
		var right jqlFilter
		if right, err = p.and(); err == nil {
			l := left
			left = func(s *jiraSite, is *JiraIssue) bool { return l(s, is) || right(s, is) }
		}
	}
	return left, err
}

func (p *jqlParser) and() (jqlFilter, error) {
	left, err := p.unary()
	for err == nil && p.keyword("AND") {
		var right jqlFilter
		if right, err = p.unary(); err == nil {
			l := left
			left = func(s *jiraSite, is *JiraIssue) bool { return l(s, is) && right(s, is) }
		}
	}
	return left, err
}

func (p *jqlParser) unary() (jqlFilter, error) {
	if t := p.peek(); t.kind == 'p' && t.text == "(" {
		p.next()
		f, err := p.or()
		if err != nil {
			return nil, err
		}
		if t := p.next(); t.kind != 'p' || t.text != ")" {
			return nil, &jqlError{"a parenthesis is not closed"}
		}
		return f, nil
	}
	return p.clause()
}

// call reads the empty argument list of a JQL function named name.
func (p *jqlParser) call(name string) error {
	if open, closed := p.next(), p.next(); open.kind != 'p' || open.text != "(" || closed.kind != 'p' || closed.text != ")" {
		return &jqlError{fmt.Sprintf("%s with arguments is not implemented", name)}
	}
	return nil
}

// list reads a parenthesized list of values of kind.
func (p *jqlParser) list(kind byte) ([]string, error) {
	if t := p.next(); t.kind != 'p' || t.text != "(" {
		return nil, &jqlError{"in is not followed by a list"}
	}
	var out []string
	for {
		t := p.next()
		if t.kind != kind {
			return nil, &jqlError{fmt.Sprintf("list value %q is not implemented", t.text)}
		}
		out = append(out, t.text)
		switch t := p.next(); {
		case t.kind == 'p' && t.text == ")":
			return out, nil
		case t.kind != 'p' || t.text != ",":
			return nil, &jqlError{"a list is not closed"}
		}
	}
}

func (p *jqlParser) clause() (jqlFilter, error) {
	field := p.next()
	name := strings.ToLower(field.text)
	op := p.next()
	isIn := op.kind == 'w' && strings.EqualFold(op.text, "in")
	if field.kind != 'w' || (op.kind != 'o' && !isIn) {
		return nil, &jqlError{fmt.Sprintf("clause at %q is not implemented", field.text)}
	}
	form := name + " " + strings.ToLower(op.text)
	if t := p.peek(); form == "sprint in" && t.kind == 'w' && strings.EqualFold(t.text, "openSprints") {
		p.next()
		if err := p.call(t.text); err != nil {
			return nil, err
		}
		return func(s *jiraSite, is *JiraIssue) bool {
			return slices.ContainsFunc(is.Sprints, func(id int64) bool { sp := s.sprint(id); return sp != nil && sp.State == "active" })
		}, nil
	}
	if form == "id in" || form == "key in" || form == "sprint in" || form == "status in" {
		kind := byte('n')
		if name == "key" {
			kind = 's'
		}
		vals, err := p.list(kind)
		if err != nil {
			return nil, err
		}
		if name == "sprint" {
			for _, v := range vals {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return nil, &jqlError{fmt.Sprintf("sprint %q is not implemented", v)}
				}
				p.sprints = append(p.sprints, n)
			}
		}
		return inFilter(name, vals), nil
	}
	v := p.next()
	switch {
	case (form == "updated >=" || form == "updated >") && v.kind == 'n':
		ms, _ := strconv.ParseInt(v.text, 10, 64)
		strict := op.text == ">"
		return func(_ *jiraSite, is *JiraIssue) bool {
			at := is.Updated.UnixMilli()
			return at > ms || (!strict && at == ms)
		}, nil
	case (form == "id >" || form == "id !=") && v.kind == 'n':
		n, _ := strconv.ParseInt(v.text, 10, 64)
		if op.text == ">" {
			return func(_ *jiraSite, is *JiraIssue) bool { return idNumber(is.ID) > n }, nil
		}
		return func(_ *jiraSite, is *JiraIssue) bool { return idNumber(is.ID) != n }, nil
	case (form == "statuscategory =" || form == "statuscategory !=") && (v.kind == 'w' || v.kind == 's'):
		cat, ok := categoryNamed(v.text)
		if !ok {
			return nil, &jqlError{fmt.Sprintf("status category %q is not implemented", v.text)}
		}
		eq := op.text == "="
		return func(s *jiraSite, is *JiraIssue) bool { return (s.status(is.Status).Category == cat) == eq }, nil
	case form == "assignee =" && v.kind == 'w' && strings.EqualFold(v.text, "currentUser"):
		if err := p.call(v.text); err != nil {
			return nil, err
		}
		if p.viewer == "" {
			return nil, &jqlError{"currentUser() where no one signed in asks is not implemented"}
		}
		who := p.viewer
		return func(_ *jiraSite, is *JiraIssue) bool { return is.Assignee == who }, nil
	case form == "status =" && v.kind == 's':
		// Jira compares a status name with case ignored (unverified); a
		// name no status has is the query's error (undefinedStatuses).
		named := v.text
		p.statuses = append(p.statuses, named)
		return func(s *jiraSite, is *JiraIssue) bool {
			st := s.status(is.Status)
			return st != nil && strings.EqualFold(st.Name, named)
		}, nil
	case form == "text ~" && v.kind == 's':
		terms, err := textTerms(v.text)
		if err != nil {
			return nil, err
		}
		return func(s *jiraSite, is *JiraIssue) bool { return s.textMatches(is, terms) }, nil
	case form == "project =" && (v.kind == 's' || v.kind == 'w' || v.kind == 'n'):
		ref := v.text
		return func(s *jiraSite, is *JiraIssue) bool {
			p := s.project(ref)
			return p != nil && p.Key == is.Project
		}, nil
	case form == "filter =" && v.kind == 'n':
		// A board's saved filter selects its project's issues, the fake's
		// only kind of filter; one no board names matches nothing here,
		// and jql/match reports it as the query's error (undefinedFilter).
		ref := v.text
		p.filters = append(p.filters, ref)
		return func(s *jiraSite, is *JiraIssue) bool {
			b := s.boardOfFilter(ref)
			return b != nil && s.filters(b, is)
		}, nil
	case (form == "hierarchylevel =" || form == "hierarchylevel !=") && v.kind == 'n':
		level, _ := strconv.Atoi(v.text)
		eq := op.text == "="
		return func(s *jiraSite, is *JiraIssue) bool { return (s.level(is) == level) == eq }, nil
	case form == "worklogauthor =" && v.kind == 's':
		who := v.text
		return func(_ *jiraSite, is *JiraIssue) bool {
			return slices.ContainsFunc(is.Worklogs, func(w JiraWorklog) bool { return w.Author == who })
		}, nil
	case (form == "worklogdate >=" || form == "worklogdate <=") && v.kind == 's' && isDate(v.text):
		day := v.text
		after := op.text == ">="
		return func(_ *jiraSite, is *JiraIssue) bool {
			return slices.ContainsFunc(is.Worklogs, func(w JiraWorklog) bool {
				d := w.Started.UTC().Format(time.DateOnly)
				return (after && d >= day) || (!after && d <= day)
			})
		}, nil
	}
	return nil, &jqlError{fmt.Sprintf("clause %s %s %s is not implemented", field.text, op.text, v.text)}
}

// inFilter keeps the issues vals name. A value that names no issue, or on
// a board list an issue not on the board, is left out with no warning
// (measured).
func inFilter(name string, vals []string) jqlFilter {
	return func(_ *jiraSite, is *JiraIssue) bool {
		switch name {
		case "id":
			return slices.Contains(vals, is.ID)
		case "key":
			return slices.ContainsFunc(vals, func(k string) bool { return strings.EqualFold(k, is.Key) })
		case "status":
			return slices.Contains(vals, is.Status)
		}
		return slices.ContainsFunc(is.Sprints, func(id int64) bool { return slices.Contains(vals, strconv.FormatInt(id, 10)) })
	}
}

// undefinedStatuses are Jira's errors for the status names q names that
// no status of the site has, as Jira refuses such a query with a 400
// (message unverified, written as the fake's other undefined values).
func (s *jiraSite) undefinedStatuses(q jqlQuery) []string {
	var out []string
	for _, name := range q.statuses {
		if !slices.ContainsFunc(s.Statuses, func(st JiraStatus) bool { return strings.EqualFold(st.Name, name) }) {
			out = append(out, fmt.Sprintf("The value '%s' does not exist for the field 'status'.", name))
		}
	}
	return out
}

// categoryNamed is the status category key a JQL value names, by name or
// key.
func categoryNamed(v string) (string, bool) {
	for key, c := range statusCategories {
		if strings.EqualFold(v, c.name) || strings.EqualFold(v, key) {
			return key, true
		}
	}
	return "", false
}

// textTerms reads a text search query: words, with every search syntax
// character escaped. An unescaped syntax character or operator word is a
// form the fake does not implement.
func textTerms(q string) ([]string, error) {
	var words []string
	var b strings.Builder
	operator := false
	flush := func() {
		if b.Len() > 0 {
			w := b.String()
			operator = operator || w == "AND" || w == "OR" || w == "NOT"
			words = append(words, strings.ToLower(w))
			b.Reset()
		}
	}
	rs := []rune(q)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			i++
			b.WriteRune(rs[i])
		case strings.ContainsRune(`+-&|!(){}[]^"~*?:\/`, r):
			return nil, &jqlError{fmt.Sprintf("text search syntax %q is not implemented", r)}
		case unicode.IsSpace(r):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	if operator {
		return nil, &jqlError{"text search operators AND, OR and NOT are not implemented"}
	}
	if len(words) == 0 {
		return nil, &jqlError{"text search for nothing is not implemented"}
	}
	return words, nil
}

// textMatches reports whether every term appears in the issue's summary,
// description, comments or text custom fields, case ignored (unverified:
// Jira's stemming and ranking are not modeled).
func (s *jiraSite) textMatches(is *JiraIssue, terms []string) bool {
	parts := []string{is.Summary, docText(is.Description)}
	for _, c := range is.Comments {
		parts = append(parts, docText(c.Body))
	}
	for id, raw := range is.Values {
		if f := s.field(id); f != nil && (f.kind == kindText || f.kind == kindDoc) {
			parts = append(parts, docText(raw), strings.Trim(string(raw), `"`))
		}
	}
	text := strings.ToLower(strings.Join(parts, " "))
	for _, t := range terms {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}

// search is the site's issues q selects, in q's order. Jira's order
// without ORDER BY is neither by ID nor by creation (measured), so the
// fake's is its own: by a hash of the ID, stable but no order a caller
// could lean on. The provider's key lookup and Jira-worklog search send
// such JQL.
func (s *jiraSite) search(q jqlQuery) []*JiraIssue {
	var out []*JiraIssue
	for i := range s.Issues {
		if is := &s.Issues[i]; q.where(s, is) {
			out = append(out, is)
		}
	}
	if len(q.order) == 0 {
		slices.SortStableFunc(out, func(a, b *JiraIssue) int { return cmp.Compare(scrambled(a.ID), scrambled(b.ID)) })
		return out
	}
	sortIssues(out, q.order)
	return out
}

// scrambled is an FNV-1a hash of id through murmur3's finalizer, so IDs
// that differ in their last digit do not hash in order.
func scrambled(id string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id)) // a hash.Hash never fails a write
	x := h.Sum32()
	x ^= x >> 16
	x *= 0x85ebca6b
	x ^= x >> 13
	x *= 0xc2b2ae35
	x ^= x >> 16
	return x
}

func sortIssues(issues []*JiraIssue, order []jqlOrder) {
	slices.SortStableFunc(issues, func(a, b *JiraIssue) int {
		for _, o := range order {
			var c int
			switch o.field {
			case "updated":
				c = a.Updated.Compare(b.Updated)
			case "created":
				c = a.Created.Compare(b.Created)
			case "id":
				c = cmp.Compare(idNumber(a.ID), idNumber(b.ID))
			case "key":
				c = cmp.Or(strings.Compare(a.Project, b.Project), cmp.Compare(keyNumber(a.Key), keyNumber(b.Key)))
			}
			if o.desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
}

func keyNumber(key string) int {
	_, num, _ := strings.Cut(key, "-")
	n, _ := strconv.Atoi(num)
	return n
}
