package trackerfake

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *jiraSite) boardJSON(b *JiraBoard) map[string]any {
	p := s.project(b.Project)
	return map[string]any{
		"id": b.ID, "self": "https://" + s.Site + "/rest/agile/1.0/board/" + strconv.FormatInt(b.ID, 10),
		"name": b.Name, "type": b.Type, "isPrivate": false,
		"location": map[string]any{
			"projectId": idNumber(p.ID), "projectKey": p.Key, "projectName": p.Name, "projectTypeKey": p.TypeKey,
			"displayName": p.Name + " (" + p.Key + ")", "name": p.Name + " (" + p.Key + ")",
		},
	}
}

// agileSprintJSON is a sprint as the agile API reads it.
func (s *jiraSite) agileSprintJSON(sp *JiraSprint) map[string]any {
	out := map[string]any{
		"id": sp.ID, "self": "https://" + s.Site + "/rest/agile/1.0/sprint/" + strconv.FormatInt(sp.ID, 10),
		"state": sp.State, "name": sp.Name, "originBoardId": sp.Board, "goal": sp.Goal,
	}
	putSprintDates(out, sp)
	return out
}

// foundBoard is the board {board} names, or the agile API's not-found.
func (c *jiraCall) foundBoard() (*JiraBoard, *answer) {
	id, err := strconv.ParseInt(c.params["board"], 10, 64)
	if b := c.s.board(id); err == nil && b != nil {
		return b, nil
	}
	a := jiraErrors(http.StatusNotFound, "Board does not exist or you do not have permission to see it.")
	return nil, &a
}

// foundSprint is the sprint {sprint} names, or the agile API's not-found.
func (c *jiraCall) foundSprint() (*JiraSprint, *answer) {
	id, err := strconv.ParseInt(c.params["sprint"], 10, 64)
	if sp := c.s.sprint(id); err == nil && sp != nil {
		return sp, nil
	}
	a := jiraErrors(http.StatusNotFound, "Sprint does not exist or you do not have permission to view it.")
	return nil, &a
}

func listBoards(c *jiraCall) answer {
	start, size, err := c.pageWindow(50, 50)
	if err != nil {
		return unhandledf("%v", err)
	}
	var types []string
	if t := c.query.Get("type"); t != "" {
		types = strings.Split(t, ",")
	}
	name := strings.ToLower(c.query.Get("name"))
	ref := c.query.Get("projectKeyOrId")
	values := []any{}
	for i := range c.s.Boards {
		b := &c.s.Boards[i]
		switch {
		case ref != "" && c.s.project(ref) != c.s.project(b.Project):
		case len(types) > 0 && !slices.Contains(types, b.Type):
		case name != "" && !strings.Contains(strings.ToLower(b.Name), name):
		default:
			values = append(values, c.s.boardJSON(b))
		}
	}
	return jsonAnswer(http.StatusOK, offsetPage(values, start, size))
}

func getBoard(c *jiraCall) answer {
	b, nf := c.foundBoard()
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, c.s.boardJSON(b))
}

func boardConfig(c *jiraCall) answer {
	b, nf := c.foundBoard()
	if nf != nil {
		return *nf
	}
	cols := []any{}
	for _, col := range b.Columns {
		sts := []any{}
		for _, id := range col.Statuses {
			sts = append(sts, map[string]any{"id": id, "self": c.s.self("status/" + id)})
		}
		cj := map[string]any{"name": col.Name, "statuses": sts}
		if col.Min > 0 {
			cj["min"] = col.Min
		}
		if col.Max > 0 {
			cj["max"] = col.Max
		}
		cols = append(cols, cj)
	}
	est := map[string]any{"type": "issueCount"}
	if f := c.s.field(b.Estimation); f != nil {
		est = map[string]any{"type": "field", "field": map[string]any{"fieldId": f.id, "displayName": f.name}}
	}
	rank := c.s.fieldOfType(rankFieldType)
	p := c.s.project(b.Project)
	return jsonAnswer(http.StatusOK, map[string]any{
		"id": b.ID, "name": b.Name, "type": b.Type,
		// Measured: the members of a project board's location, id a
		// string. The fake's boards are all on a project.
		"location":     map[string]any{"type": "project", "id": p.ID, "key": p.Key, "name": p.Name, "self": c.s.self("project/" + p.ID)},
		"filter":       map[string]any{"id": b.FilterID, "self": c.s.self("filter/" + b.FilterID)},
		"subQuery":     map[string]any{"query": ""},
		"columnConfig": map[string]any{"columns": cols, "constraintType": "none"},
		"estimation":   est,
		"ranking":      map[string]any{"rankCustomFieldId": rank.Schema.CustomID},
	})
}

func boardSprints(c *jiraCall) answer {
	b, nf := c.foundBoard()
	if nf != nil {
		return *nf
	}
	start, size, err := c.pageWindow(50, 50)
	if err != nil {
		return unhandledf("%v", err)
	}
	var states []string
	if st := c.query.Get("state"); st != "" {
		states = strings.Split(st, ",")
	}
	values := []any{}
	for i := range c.s.Sprints {
		sp := &c.s.Sprints[i]
		if (sp.Board == b.ID || slices.Contains(sp.Shown, b.ID)) && (len(states) == 0 || slices.Contains(states, sp.State)) {
			values = append(values, c.s.agileSprintJSON(sp))
		}
	}
	return jsonAnswer(http.StatusOK, offsetPage(values, start, size))
}

func getSprint(c *jiraCall) answer {
	sp, nf := c.foundSprint()
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, c.s.agileSprintJSON(sp))
}

// sprintBody is a sprint create or partial update.
type sprintBody struct {
	Name          *string `json:"name"`
	OriginBoardID *int64  `json:"originBoardId"`
	Goal          *string `json:"goal"`
	StartDate     *string `json:"startDate"`
	EndDate       *string `json:"endDate"`
	State         *string `json:"state"`
}

// dates reads the body's dates over sp's.
func (b sprintBody) dates(sp *JiraSprint) map[string]string {
	for _, d := range []struct {
		v   *string
		out *time.Time
		key string
	}{{b.StartDate, &sp.Start, "startDate"}, {b.EndDate, &sp.End, "endDate"}} {
		if d.v == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, *d.v)
		if err != nil {
			return map[string]string{d.key: "Invalid date format."}
		}
		*d.out = t.UTC()
	}
	if !sp.Start.IsZero() && !sp.End.IsZero() && !sp.End.After(sp.Start) {
		return map[string]string{"endDate": "The end date must be after the start date."}
	}
	return nil
}

func createSprint(c *jiraCall) answer {
	var in sprintBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.State != nil {
		return unhandledf("a new sprint's state")
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" || in.OriginBoardID == nil {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"name": "The sprint name and board are required."})
	}
	b := c.s.board(*in.OriginBoardID)
	if b == nil {
		return jiraErrors(http.StatusNotFound, "Board does not exist or you do not have permission to see it.")
	}
	if b.Type != "scrum" {
		return jiraErrors(http.StatusBadRequest, "The board does not support sprints.")
	}
	sp := JiraSprint{ID: c.e.nextID(), Board: b.ID, Name: *in.Name, State: "future"}
	if in.Goal != nil {
		sp.Goal = *in.Goal
	}
	if refused := in.dates(&sp); refused != nil {
		return jiraFieldErrors(http.StatusBadRequest, refused)
	}
	c.s.Sprints = append(c.s.Sprints, sp)
	return jsonAnswer(http.StatusCreated, c.s.agileSprintJSON(&c.s.Sprints[len(c.s.Sprints)-1]))
}

// updateSprint changes the members the body names: future to active
// starts the sprint (both dates needed), active to closed completes it;
// no other state change is allowed (documented). A closed sprint's dates
// are kept as they are.
func updateSprint(c *jiraCall) answer {
	var in sprintBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.OriginBoardID != nil {
		return unhandledf("moving a sprint to another board")
	}
	sp, nf := c.foundSprint()
	if nf != nil {
		return *nf
	}
	next := *sp
	if in.Name != nil {
		next.Name = *in.Name
	}
	if in.Goal != nil {
		next.Goal = *in.Goal
	}
	if sp.State != "closed" {
		if refused := in.dates(&next); refused != nil {
			return jiraFieldErrors(http.StatusBadRequest, refused)
		}
	}
	if in.State != nil && *in.State != sp.State {
		switch {
		case sp.State == "future" && *in.State == "active":
			if next.Start.IsZero() || next.End.IsZero() {
				return jiraErrors(http.StatusBadRequest, "A sprint needs a start and end date to start.")
			}
		case sp.State == "active" && *in.State == "closed":
			next.Complete = c.e.now()
		default:
			return jiraErrors(http.StatusBadRequest, "A sprint cannot move from "+sp.State+" to "+*in.State+".")
		}
		next.State = *in.State
	}
	*sp = next
	return jsonAnswer(http.StatusOK, c.s.agileSprintJSON(sp))
}

// deleteSprint removes a sprint; its issues leave it (documented: open
// issues move to the backlog).
func deleteSprint(c *jiraCall) answer {
	sp, nf := c.foundSprint()
	if nf != nil {
		return *nf
	}
	id := sp.ID
	for i := range c.s.Issues {
		is := &c.s.Issues[i]
		if slices.Contains(is.Sprints, id) {
			before := cloneIssue(*is)
			is.Sprints = slices.DeleteFunc(is.Sprints, func(x int64) bool { return x == id })
			c.sprintChanged(is, &before)
		}
	}
	c.s.Sprints = slices.DeleteFunc(c.s.Sprints, func(x JiraSprint) bool { return x.ID == id })
	return emptyAnswer(http.StatusNoContent)
}

// sprintChanged records is's sprint change from before.
func (c *jiraCall) sprintChanged(is, before *JiraIssue) {
	f := c.s.field(c.s.fieldOfType(sprintFieldType).ID)
	if item, ok := c.s.changeItem(f, before, is); ok {
		c.changed(is, []JiraChangeItem{item})
	}
}

// estimateText is the value an estimation answer gives for an original
// estimate: whole hours as Nh, as Jira writes timetracking.originalEstimate
// (measured), anything else as Nm. Unverified: that the answer uses the
// same text, and any mixed hours and minutes form (none was measured).
// Days and weeks, which Jira also writes, the fake never does.
func estimateText(secs int64) string {
	if secs%3600 == 0 {
		return strconv.FormatInt(secs/3600, 10) + "h"
	}
	return strconv.FormatInt(secs/60, 10) + "m"
}

func setEstimation(c *jiraCall) answer {
	var in struct {
		Value *string `json:"value"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	id, err := strconv.ParseInt(c.query.Get("boardId"), 10, 64)
	b := c.s.board(id)
	if err != nil || b == nil {
		return jiraErrors(http.StatusBadRequest, "The board ID is required and must name a board.")
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	f := c.s.field(b.Estimation)
	if f == nil {
		return jiraErrors(http.StatusBadRequest, "The board does not estimate in a field.")
	}
	before := cloneIssue(*is)
	var stored any
	switch {
	case f.id == "timeoriginalestimate":
		var secs *int64
		if in.Value != nil {
			n, err := jiraDuration(*in.Value)
			if err != nil {
				return jiraErrors(http.StatusBadRequest, "The estimate is not a duration.")
			}
			secs, stored = &n, estimateText(n)
		}
		is.OriginalEstimate = secs
		if secs != nil && is.RemainingEstimate == nil {
			remaining := *secs
			is.RemainingEstimate = &remaining
		}
	case f.kind == kindNumber:
		var n *float64
		if in.Value != nil {
			v, err := strconv.ParseFloat(*in.Value, 64)
			if err != nil {
				return jiraErrors(http.StatusBadRequest, "The estimate is not a number.")
			}
			n, stored = &v, strconv.FormatFloat(v, 'f', -1, 64)
		}
		delete(is.Values, f.id)
		if n != nil {
			c.s.store(is, f, false, mustJSON(*n))
		}
	default:
		return unhandledf("estimation in field %s of kind %d", f.id, f.kind)
	}
	var items []JiraChangeItem
	for _, id := range []string{f.id, "timeestimate"} {
		if item, ok := c.s.changeItem(c.s.field(id), &before, is); ok {
			items = append(items, item)
		}
	}
	c.changed(is, items)
	return jsonAnswer(http.StatusOK, map[string]any{"fieldId": f.id, "value": stored})
}
