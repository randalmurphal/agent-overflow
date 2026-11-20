package trackerfake

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// archivable is an object a connection leaves out unless includeArchived
// is set: an archived entity, or a trashed issue (measured for issues).
type archivable interface{ archived() bool }

// disableable is a user a connection leaves out unless includeDisabled.
type disableable interface{ disabled() bool }

// sortable is an object with sort keys (sortTables): a sort field's
// value.
type sortable interface {
	sortKey(field string) (float64, bool)
}

// connection pages list, a connection of schema type typ, by the
// arguments in connectionArgs, each consumed from args.
func (r *gqlRun) connection(typ string, owner gqlObject, name string, list []gqlObject, args map[string]any) (connObj, *gqlErr) {
	includeArchived, _ := take(args, "includeArchived")
	includeDisabled, _ := take(args, "includeDisabled")
	var kept []gqlObject
	for _, o := range list {
		if a, ok := o.(archivable); ok && a.archived() && includeArchived != true {
			continue
		}
		if d, ok := o.(disableable); ok && d.disabled() && includeDisabled != true {
			continue
		}
		kept = append(kept, o)
	}
	if filter, ok := take(args, "filter"); ok {
		f, _ := filter.(map[string]any)
		kept = slices.DeleteFunc(kept, func(o gqlObject) bool { return !r.matches(o, f) })
	}
	keyOf := r.order(owner, name, kept, args)
	first := 50
	if v, ok := take(args, "first"); ok {
		first = int(toFloat(v))
	}
	// Measured: the largest page is 250, outer or nested; a larger one
	// answers HTTP 200, data null and one INVALID_INPUT error "Argument
	// Validation Error" at the field. The client reads
	// extensions.userPresentableMessage, which carried "first must not be
	// greater than 250" (measured earlier).
	if first > linearMaxPage || first < 0 {
		return connObj{}, &gqlErr{code: "INVALID_INPUT", message: "Argument Validation Error",
			presentable: fmt.Sprintf("first must not be greater than %d", linearMaxPage), userError: true}
	}
	start := 0
	if after, ok := take(args, "after"); ok {
		key, id, okc := readCursor(str(after))
		if !okc {
			return connObj{}, &gqlErr{code: "INVALID_INPUT", message: "Invalid cursor", userError: true}
		}
		start = len(kept)
		for i, o := range kept {
			if cmpKey(keyOf(o), objID(o), key, id) > 0 {
				start = i
				break
			}
		}
	}
	end := min(start+first, len(kept))
	if !chargedByResults(owner, name) {
		r.cost += first - (end - start)
	}
	page := connObj{typ: typ, nodes: kept[start:end], hasNext: end < len(kept)}
	if end > start {
		last := kept[end-1]
		page.end = cursorOf(keyOf(last), objID(last))
	}
	return page, nil
}

// chargedByResults reports a connection Linear charges by the nodes it
// returns: an issue list and any list nested in an issue. Every other
// (catalog) connection is charged by its page size, first (measured, see
// docs/references/tracker-api-measurements.md, Linear; the points per
// node are the fake's own count, unverified against Linear's rules).
func chargedByResults(owner gqlObject, name string) bool {
	if _, ok := owner.(issueObj); ok {
		return true
	}
	return name == "issues" || name == "searchIssues" || name == "children"
}

// order sorts list by the sort argument (sortSpec), else keeps its
// order, and returns each object's keyset key.
func (r *gqlRun) order(owner gqlObject, name string, list []gqlObject, args map[string]any) func(gqlObject) []float64 {
	index := map[string]float64{}
	for i, o := range list {
		index[objID(o)] = float64(i)
	}
	keyOf := func(o gqlObject) []float64 { return []float64{index[objID(o)]} }
	sortArg, ok := take(args, "sort")
	if !ok {
		return keyOf
	}
	field, descending, problem := sortSpec(sortArg)
	if problem != "" {
		r.fail("%s.%s: %s", owner.typeName(), name, problem)
		return keyOf
	}
	sign := 1.0
	if descending {
		sign = -1
	}
	keyOf = func(o gqlObject) []float64 {
		var k float64
		s, ok := o.(sortable)
		if ok {
			k, ok = s.sortKey(field)
		}
		if !ok {
			r.fail("sort of %s by %s is not implemented", o.typeName(), field)
		}
		return []float64{sign * k}
	}
	slices.SortStableFunc(list, func(a, b gqlObject) int { return cmpKey(keyOf(a), objID(a), keyOf(b), objID(b)) })
	return keyOf
}

func cmpKey(a []float64, aid string, b []float64, bid string) int {
	for i := range min(len(a), len(b)) {
		if c := cmp.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return strings.Compare(aid, bid)
}

func objID(o gqlObject) string {
	v, _ := o.value("id")
	return str(v)
}

type cursor struct {
	Key []float64 `json:"k"`
	ID  string    `json:"i"`
}

func cursorOf(key []float64, id string) string {
	raw, _ := json.Marshal(cursor{key, id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func readCursor(s string) ([]float64, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return nil, "", false
	}
	return c.Key, c.ID, true
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// matches evaluates a filter input object on o: and, or, and per field a
// comparator or a nested filter, read through o's field table. The
// pre-pass has refused a filter key or comparator the tables lack; a
// failure here is the two disagreeing.
func (r *gqlRun) matches(o gqlObject, filter map[string]any) bool {
	for key, cond := range filter {
		if cond == nil {
			continue
		}
		switch key {
		case "and", "or":
			list, _ := cond.([]any)
			some, all := false, true
			for _, sub := range list {
				m, _ := sub.(map[string]any)
				hit := r.matches(o, m)
				some, all = some || hit, all && hit
			}
			if (key == "and" && !all) || (key == "or" && !some) {
				return false
			}
			continue
		}
		v, ok := o.value(key)
		if !ok {
			r.fail("filter field %s.%s is not implemented", o.typeName(), key)
			return false
		}
		c, _ := cond.(map[string]any)
		if !r.compare(o.typeName()+"."+key, v, c) {
			return false
		}
	}
	return true
}

// compare applies a comparator, or a nested filter on an object value.
func (r *gqlRun) compare(what string, v any, c map[string]any) bool {
	if null, ok := c["null"]; ok && len(c) == 1 {
		return (v == nil) == (null == true)
	}
	switch v := v.(type) {
	case nil:
		// A nested filter on a relation that is not set matches nothing
		// (measured: team {id {eq}} returns no workspace label, while
		// team {null: true}, read above, returns them); a comparator on
		// an unset scalar is read below.
		for op := range c {
			if _, ok := comparators[op]; !ok {
				return false
			}
		}
	case gqlObject:
		return r.matches(v, c)
	case []gqlObject:
		some, ok := c["some"].(map[string]any)
		if !ok || len(c) != 1 {
			r.fail("filter of %s other than some is not implemented", what)
			return false
		}
		return slices.ContainsFunc(v, func(o gqlObject) bool { return r.matches(o, some) })
	case []string:
		r.fail("filter of the list %s is not implemented", what)
		return false
	}
	for op, want := range c {
		if !r.compareScalar(what, op, v, want) {
			return false
		}
	}
	return true
}

func (r *gqlRun) compareScalar(what, op string, v, want any) bool {
	if _, ok := v.(time.Time); ok {
		if s, ok := want.(string); ok {
			w, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				r.fail("filter of %s: %q is not a time", what, s)
				return false
			}
			want = w
		}
	}
	f, ok := comparators[op]
	if !ok {
		r.fail("comparator %s of %s is not implemented", op, what)
		return false
	}
	return f(normalize(v), normalize(want))
}
