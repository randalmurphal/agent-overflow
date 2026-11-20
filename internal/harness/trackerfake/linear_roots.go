package trackerfake

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// rootSpec is what a root field with arguments of its own reads: its
// arguments (a connection's own are connectionArgs), the input object
// type of its input argument, and a mutation payload's entity field.
type rootSpec struct {
	args    []string
	input   string
	payload payloadFields
}

// rootField is a root field with arguments of its own and its resolver;
// a mutation's resolver returns its payload's entity.
type rootField[T any] struct {
	rootSpec
	run func(o T, r *gqlRun, args map[string]any) (any, *gqlErr)
}

// rootFields is a root type's fields: those without arguments of their
// own, read as any object's, and those with.
type rootFields[T any] struct {
	plain fieldTable[T]
	args  map[string]rootField[T]
}

func (f rootFields[T]) has(name string) bool { _, ok := f.args[name]; return ok || f.plain.has(name) }

func (f rootFields[T]) names() []string {
	return slices.Sorted(slices.Values(append(f.plain.names(), slices.Collect(maps.Keys(f.args))...)))
}

// rootSpecOf is the spec of typ's field name when it is a root field with
// arguments of its own.
func rootSpecOf(typ, name string) (rootSpec, bool) {
	switch typ {
	case "Query":
		f, ok := queryArgs[name]
		return f.rootSpec, ok
	case "Mutation":
		f, ok := mutations[name]
		return f.rootSpec, ok
	}
	return rootSpec{}, false
}

// queryRoot is the Query type.
type queryRoot struct{ w *linearWorkspace }

func (queryRoot) typeName() string                { return "Query" }
func (q queryRoot) value(name string) (any, bool) { return queryLists.get(q, name) }

var queryFields = rootFields[queryRoot]{plain: queryLists, args: queryArgs}

// listOf is a root list of every record in all, which the connection
// then filters and pages.
func listOf[T any](all []T, obj func(*T) gqlObject) []gqlObject {
	out := make([]gqlObject, len(all))
	for i := range all {
		out[i] = obj(&all[i])
	}
	return out
}

var queryLists = fieldTable[queryRoot]{
	"viewer":       func(q queryRoot) any { return q.w.userRef(q.w.Viewer) },
	"organization": func(q queryRoot) any { return orgObj(q) },
	"users": func(q queryRoot) any {
		return listOf(q.w.Users, func(u *LinearUser) gqlObject { return userObj{q.w, u} })
	},
	"teams": func(q queryRoot) any {
		return listOf(q.w.Teams, func(t *LinearTeam) gqlObject { return teamObj{q.w, t} })
	},
	"workflowStates": func(q queryRoot) any {
		return listOf(q.w.States, func(s *LinearState) gqlObject { return stateObj{q.w, s} })
	},
	"cycles": func(q queryRoot) any {
		return listOf(q.w.Cycles, func(c *LinearCycle) gqlObject { return cycleObj{q.w, c} })
	},
	"issueLabels": func(q queryRoot) any {
		return listOf(q.w.Labels, func(l *LinearLabel) gqlObject { return labelObj{q.w, l} })
	},
	"projects": func(q queryRoot) any {
		return listOf(q.w.Projects, func(p *LinearProject) gqlObject { return projectObj{q.w, p} })
	},
	"issues": func(q queryRoot) any {
		return listOf(q.w.Issues, func(is *LinearIssue) gqlObject { return issueObj{w: q.w, is: is} })
	},
}

// byID is a root read of one record by its id argument: the record's
// object, or Linear's not-found for typ.
func byID(typ string, find func(w *linearWorkspace, id string) any) rootField[queryRoot] {
	return rootField[queryRoot]{rootSpec: rootSpec{args: []string{"id"}}, run: func(q queryRoot, _ *gqlRun, args map[string]any) (any, *gqlErr) {
		if v := find(q.w, idArg(args)); v != nil {
			return v, nil
		}
		return nil, notFound(typ)
	}}
}

var queryArgs = map[string]rootField[queryRoot]{
	"issue": byID("Issue", (*linearWorkspace).issueRef),
	"comment": byID("Comment", func(w *linearWorkspace, id string) any {
		if c := w.comment(id); c != nil {
			return commentObj{w, c}
		}
		return nil
	}),
	"issueRelation": byID("IssueRelation", func(w *linearWorkspace, id string) any {
		if rel := w.relation(id); rel != nil {
			return relationObj{w, rel}
		}
		return nil
	}),
	"cycle":        byID("Cycle", (*linearWorkspace).cycleRef),
	"team":         byID("Team", (*linearWorkspace).teamRef),
	"searchIssues": {rootSpec: rootSpec{args: []string{"term", "teamId"}}, run: queryRoot.search},
}

// notFound is Linear's error for an ID that names nothing, at the field:
// an issue's is "Entity not found: Issue", answered HTTP 200 with data
// null; a comment's, relation's, cycle's or team's says "Could not find
// referenced" and the type (measured).
func notFound(typ string) *gqlErr {
	msg := "Could not find referenced " + typ + "."
	if typ == "Issue" {
		msg = "Entity not found: Issue"
	}
	return &gqlErr{code: "INPUT_ERROR", message: msg, userError: true}
}

func (q queryRoot) argField(r *gqlRun, f *ast.Field, args map[string]any) (any, *gqlErr, bool) {
	spec, ok := queryArgs[f.Name]
	if !ok {
		return nil, nil, false
	}
	v, err := spec.run(q, r, args)
	return v, err, true
}

// search is the issues whose identifier, title or description holds the
// term, case ignored, newest update first (unverified: Linear ranks by
// relevance), in the team when teamId is set.
func (q queryRoot) search(_ *gqlRun, args map[string]any) (any, *gqlErr) {
	w := q.w
	term, _ := take(args, "term")
	team, _ := take(args, "teamId")
	t := strings.ToLower(str(term))
	var hits []*LinearIssue
	for i := range w.Issues {
		is := &w.Issues[i]
		text := strings.ToLower(w.identifier(is) + " " + is.Title + " " + is.Description)
		if strings.Contains(text, t) && (team == nil || team == is.Team) {
			hits = append(hits, is)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].UpdatedAt.After(hits[j].UpdatedAt) })
	out := make([]gqlObject, len(hits))
	for i, is := range hits {
		out[i] = issueObj{w: w, is: is, search: true}
	}
	return out, nil
}

// mutationRoot is the Mutation type.
type mutationRoot struct{ w *linearWorkspace }

func (mutationRoot) typeName() string         { return "Mutation" }
func (mutationRoot) value(string) (any, bool) { return nil, false }

var mutationFields = rootFields[mutationRoot]{args: mutations}

// mutationOf is a mutation's spec.
func mutationOf(input string, payload payloadFields, run func(mutationRoot, *gqlRun, map[string]any) (any, *gqlErr), args ...string) rootField[mutationRoot] {
	if input != "" {
		args = append(args, "input")
	}
	return rootField[mutationRoot]{rootSpec: rootSpec{args: args, input: input, payload: payload}, run: run}
}

var mutations = map[string]rootField[mutationRoot]{
	"issueCreate":                  mutationOf("IssueCreateInput", "issue", mutationRoot.createIssue),
	"issueUpdate":                  mutationOf("IssueUpdateInput", "issue", mutationRoot.updateIssue, "id"),
	"issueArchive":                 mutationOf("", "entity", mutationRoot.archiveIssue, "id"),
	"issueUnarchive":               mutationOf("", "entity", mutationRoot.unarchiveIssue, "id"),
	"issueDelete":                  mutationOf("", "", mutationRoot.deleteIssue, "id", "permanentlyDelete"),
	"commentCreate":                mutationOf("CommentCreateInput", "comment", mutationRoot.createComment),
	"commentUpdate":                mutationOf("CommentUpdateInput", "comment", mutationRoot.updateComment, "id"),
	"commentDelete":                mutationOf("", "", mutationRoot.deleteComment, "id"),
	"issueRelationCreate":          mutationOf("IssueRelationCreateInput", "issueRelation", mutationRoot.createRelation),
	"issueRelationDelete":          mutationOf("", "", mutationRoot.deleteRelation, "id"),
	"attachmentCreate":             mutationOf("AttachmentCreateInput", "attachment", mutationRoot.createAttachment),
	"cycleStartUpcomingCycleToday": mutationOf("", "cycle", mutationRoot.startCycle, "id"),
}

// argField runs a mutation in its own transaction: a mutation Linear
// refuses with an error changes nothing, while the document's other
// mutations stand, as each root mutation field resolves on its own.
func (m mutationRoot) argField(r *gqlRun, f *ast.Field, args map[string]any) (any, *gqlErr, bool) {
	spec, ok := mutations[f.Name]
	if !ok {
		return nil, nil, false
	}
	undo, err := r.e.saveLinear(m.w)
	if err != nil {
		r.fail("%v", err)
		return nil, nil, true
	}
	entity, gerr := spec.run(m, r, args)
	if gerr != nil {
		undo()
		return nil, gerr, true
	}
	return payloadObj{typ: f.Definition.Type.Name(), field: spec.payload, entity: entity}, nil, true
}
