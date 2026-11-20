package trackerfake

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The tables the Linear executor answers from. The resolvers read them
// and the pre-pass (linear_check.go) checks a request against them
// before anything runs, so the two never disagree on what is
// implemented.

// fieldSet is the fields of one object type the executor answers.
type fieldSet interface {
	has(name string) bool
	names() []string
}

// fieldTable is an object type's fields, each with its resolver: nil, a
// string, float64, bool or time.Time scalar, a []string, an object, or a
// list of objects for a list or connection.
type fieldTable[T any] map[string]func(T) any

func (t fieldTable[T]) get(o T, name string) (any, bool) {
	f, ok := t[name]
	if !ok {
		return nil, false
	}
	return f(o), true
}

func (t fieldTable[T]) has(name string) bool { _, ok := t[name]; return ok }

func (t fieldTable[T]) names() []string { return slices.Sorted(maps.Keys(t)) }

// linearTypes are the object types the executor answers, by schema name.
// A connection type is answered by connFields and a mutation payload by
// its mutation's payloadFields.
var linearTypes = map[string]fieldSet{
	"Query": queryFields, "Mutation": mutationFields, "User": userFields, "Organization": orgFields, "Team": teamFields,
	"WorkflowState": stateFields, "Cycle": cycleFields, "IssueLabel": labelFields, "Project": projectFields,
	"ProjectStatus": projectStatusFields, "Issue": issueFields, "IssueSearchResult": issueFields,
	"Comment": commentFields, "Attachment": attachmentFields, "IssueRelation": relationFields,
	"IssueHistory": historyFields, "IssueStateSpan": spanFields, "PageInfo": pageInfoFields,
}

// linearType is the field set the executor answers typ with; nil for a
// type it does not answer.
func linearType(typ string) fieldSet {
	if isConnection(typ) {
		return connFields
	}
	return linearTypes[typ]
}

// isConnection reports a type the executor pages: a connection, or the
// issue search's payload.
func isConnection(typ string) bool {
	return strings.HasSuffix(typ, "Connection") || typ == "IssueSearchPayload"
}

// connectionArgs are the arguments connection reads.
var connectionArgs = []string{"filter", "includeArchived", "includeDisabled", "sort", "first", "after"}

// connObj is one page of a connection of schema type typ.
type connObj struct {
	typ     string
	nodes   []gqlObject
	hasNext bool
	end     string
}

func (c connObj) typeName() string { return c.typ }

func (c connObj) value(name string) (any, bool) { return connFields.get(c, name) }

var connFields = fieldTable[connObj]{
	"nodes":    func(c connObj) any { return c.nodes },
	"pageInfo": func(c connObj) any { return pageInfoObj(c) },
}

type pageInfoObj connObj

func (pageInfoObj) typeName() string { return "PageInfo" }

func (p pageInfoObj) value(name string) (any, bool) { return pageInfoFields.get(p, name) }

var pageInfoFields = fieldTable[pageInfoObj]{
	"hasNextPage": func(p pageInfoObj) any { return p.hasNext },
	"endCursor": func(p pageInfoObj) any {
		if p.end == "" {
			return nil
		}
		return p.end
	},
}

// payloadFields is a mutation payload's fields: success, lastSyncId and
// the entity field it names, if any.
type payloadFields string

func (p payloadFields) has(name string) bool {
	return name == "success" || name == "lastSyncId" || (p != "" && name == string(p))
}

func (p payloadFields) names() []string {
	if p == "" {
		return []string{"lastSyncId", "success"}
	}
	return []string{"lastSyncId", "success", string(p)}
}

// payloadObj is a mutation's payload of schema type typ.
type payloadObj struct {
	typ    string
	field  payloadFields
	entity any
}

func (p payloadObj) typeName() string { return p.typ }

func (p payloadObj) value(name string) (any, bool) {
	switch {
	case !p.field.has(name):
		return nil, false
	case name == "success":
		return true, true
	case name == "lastSyncId":
		return 1.0, true
	}
	return p.entity, true
}

// sortTables are each sortable type's sort keys: a sort field's value.
var sortTables = map[string]map[string]func(*LinearIssue) float64{
	"Issue": issueSorts,
}

// issueSorts are the issue sort keys.
var issueSorts = map[string]func(*LinearIssue) float64{
	"updatedAt": func(is *LinearIssue) float64 { return float64(is.UpdatedAt.UnixMilli()) },
	"createdAt": func(is *LinearIssue) float64 { return float64(is.CreatedAt.UnixMilli()) },
	"manual":    func(is *LinearIssue) float64 { return is.SortOrder },
	"priority": func(is *LinearIssue) float64 {
		// No priority sorts after low (unverified).
		if is.Priority == 0 {
			return 5
		}
		return is.Priority
	},
}

// sortSpec reads a sort argument the executor implements: one entry of
// one field whose direction has an order only. problem names anything
// else.
func sortSpec(arg any) (field string, descending bool, problem string) {
	entries, _ := arg.([]any)
	if len(entries) != 1 {
		return "", false, "a sort of " + strconv.Itoa(len(entries)) + " entries is not implemented"
	}
	spec, _ := entries[0].(map[string]any)
	if len(spec) != 1 {
		return "", false, "a sort entry of " + strconv.Itoa(len(spec)) + " fields is not implemented"
	}
	for f, dir := range spec {
		d, _ := dir.(map[string]any)
		order, _ := d["order"].(string)
		if len(d) != 1 || (order != "Ascending" && order != "Descending") {
			return "", false, "sort " + f + " other than by order alone is not implemented"
		}
		field, descending = f, order == "Descending"
	}
	return field, descending, ""
}
