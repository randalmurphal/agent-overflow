package trackerfake

import (
	"sort"
	"strings"
)

// queryRoot is the Query type.
type queryRoot struct{ w *linearWorkspace }

func (queryRoot) typeName() string { return "Query" }

// value is a root list, which the connection then filters and pages.
func (q queryRoot) value(name string) (any, bool) {
	w := q.w
	var out []gqlObject
	switch name {
	case "viewer":
		return userObj{w, w.user(w.Viewer)}, true
	case "organization":
		return orgObj{w}, true
	case "users":
		for i := range w.Users {
			out = append(out, userObj{w, &w.Users[i]})
		}
	case "teams":
		for i := range w.Teams {
			out = append(out, teamObj{w, &w.Teams[i]})
		}
	case "workflowStates":
		for i := range w.States {
			out = append(out, stateObj{w, &w.States[i]})
		}
	case "cycles":
		for i := range w.Cycles {
			out = append(out, cycleObj{w, &w.Cycles[i]})
		}
	case "issueLabels":
		for i := range w.Labels {
			out = append(out, labelObj{w, &w.Labels[i]})
		}
	case "projects":
		for i := range w.Projects {
			out = append(out, projectObj{w, &w.Projects[i]})
		}
	case "issues":
		for i := range w.Issues {
			out = append(out, issueObj{w: w, is: &w.Issues[i]})
		}
	default:
		return nil, false
	}
	return out, true
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

func (q queryRoot) argField(r *gqlRun, name string, args map[string]any) (any, *gqlErr, bool) {
	w := q.w
	switch name {
	case "issue", "comment", "issueRelation", "cycle", "team":
	case "searchIssues":
		return q.search(args), nil, true
	default:
		return nil, nil, false
	}
	raw, _ := take(args, "id")
	id, _ := raw.(string)
	var v any
	typ := map[string]string{"issue": "Issue", "comment": "Comment", "issueRelation": "IssueRelation", "cycle": "Cycle", "team": "Team"}[name]
	switch name {
	case "issue":
		v = w.issueRef(id)
	case "comment":
		if c := w.comment(id); c != nil {
			v = commentObj{w, c}
		}
	case "issueRelation":
		if rel := w.relation(id); rel != nil {
			v = relationObj{w, rel}
		}
	case "cycle":
		v = w.cycleRef(id)
	case "team":
		v = w.teamRef(id)
	}
	if v == nil {
		return nil, notFound(typ), true
	}
	return v, nil, true
}

// search is the issues whose identifier, title or description holds the
// term, case ignored, newest update first (unverified: Linear ranks by
// relevance), in the team when teamId is set.
func (q queryRoot) search(args map[string]any) []gqlObject {
	w := q.w
	term, _ := take(args, "term")
	team, _ := take(args, "teamId")
	t := strings.ToLower(term.(string))
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
	return out
}

// payloadObj is a mutation's payload: success, and the entity under the
// payload's own field name.
type payloadObj struct {
	typ    string
	field  string
	entity any
}

func (p payloadObj) typeName() string { return p.typ }

func (p payloadObj) value(name string) (any, bool) {
	switch name {
	case "success":
		return true, true
	case "lastSyncId":
		return 1.0, true
	case p.field:
		if p.field != "" {
			return p.entity, true
		}
	}
	return nil, false
}

// mutationRoot is the Mutation type.
type mutationRoot struct{ w *linearWorkspace }

func (mutationRoot) typeName() string         { return "Mutation" }
func (mutationRoot) value(string) (any, bool) { return nil, false }

func (m mutationRoot) argField(r *gqlRun, name string, args map[string]any) (any, *gqlErr, bool) {
	writes := map[string]func(*gqlRun, map[string]any) (any, *gqlErr){
		"issueCreate":                  m.createIssue,
		"issueUpdate":                  m.updateIssue,
		"issueArchive":                 m.archiveIssue,
		"issueUnarchive":               m.unarchiveIssue,
		"issueDelete":                  m.deleteIssue,
		"commentCreate":                m.createComment,
		"commentUpdate":                m.updateComment,
		"commentDelete":                m.deleteComment,
		"issueRelationCreate":          m.createRelation,
		"issueRelationDelete":          m.deleteRelation,
		"attachmentCreate":             m.createAttachment,
		"cycleStartUpcomingCycleToday": m.startCycle,
	}
	write, ok := writes[name]
	if !ok {
		return nil, nil, false
	}
	v, err := write(r, args)
	return v, err, true
}
