package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// SprintState is where a sprint is in its life: future, then active, then
// closed. A value outside the constants below is refused as a filter
// before anything is sent.
type SprintState string

const (
	SprintFuture SprintState = "future"
	SprintActive SprintState = "active"
	SprintClosed SprintState = "closed"
)

func (s SprintState) valid() bool {
	return s == SprintFuture || s == SprintActive || s == SprintClosed
}

// sprintTimeLayout is how a sprint's dates are written: the spec's
// examples carry milliseconds and an offset with a colon.
const sprintTimeLayout = "2006-01-02T15:04:05.000Z07:00"

type sprintJSON struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	State    SprintState `json:"state"`
	BoardID  int64       `json:"originBoardId"`
	Goal     string      `json:"goal"`
	Start    string      `json:"startDate"`
	End      string      `json:"endDate"`
	Complete string      `json:"completeDate"`
}

// sprint decodes s; a date that does not parse is one of its Problems.
func (s sprintJSON) sprint() Sprint {
	out := Sprint{ID: s.ID, Name: s.Name, State: s.State, BoardID: s.BoardID, Goal: s.Goal}
	timeField("startDate", s.Start, &out.Start, &out.Problems)
	timeField("endDate", s.End, &out.End, &out.Problems)
	timeField("completeDate", s.Complete, &out.Done, &out.Problems)
	return out
}

func sprintPath(id int64) (string, error) {
	if err := checkAgileID("sprint", id); err != nil {
		return "", err
	}
	return agilePath + "sprint/" + strconv.FormatInt(id, 10), nil
}

// SprintQuery picks a board's sprints.
type SprintQuery struct {
	Board int64
	// States keeps the sprints in these states; none keeps all.
	States []SprintState
	// Limit stops after that many sprints; zero reads every page.
	Limit int
}

// SprintList is the sprints a listing returned, closed first, then
// active, then future, each in backlog order. Complete is false when the
// limit cut the listing. Read is Jira's clock when it answered the
// listing's first page, zero when the answer named none.
type SprintList struct {
	Sprints  []Sprint
	Complete bool
	Read     time.Time
	// mark is the credential read the listing was drawn under.
	mark trackerapi.CredentialMark
}

func cloneSprintList(l SprintList) SprintList {
	l.Sprints = slices.Clone(l.Sprints)
	for i := range l.Sprints {
		l.Sprints[i].Problems = slices.Clone(l.Sprints[i].Problems)
	}
	return l
}

// Sprints lists a board's sprints page by page to the end or q.Limit. A
// failure after some pages returns them with the error.
func (c *Client) Sprints(ctx context.Context, q SprintQuery) (SprintList, error) {
	if err := c.admit(ctx); err != nil {
		return SprintList{}, err
	}
	return c.sprints(ctx, q)
}

func (c *Client) sprints(ctx context.Context, q SprintQuery) (SprintList, error) {
	if err := checkAgileID("board", q.Board); err != nil {
		return SprintList{}, err
	}
	if q.Limit < 0 {
		return SprintList{}, notSent("sprints of board %d: a negative limit", q.Board)
	}
	v := url.Values{}
	var states []string
	for _, s := range q.States {
		if !s.valid() {
			return SprintList{}, notSent("sprints of board %d: %q is not a SprintState", q.Board, s)
		}
		states = append(states, string(s))
	}
	if len(states) > 0 {
		v.Set("state", strings.Join(states, ","))
	}
	path := agilePath + "board/" + strconv.FormatInt(q.Board, 10) + "/sprint"
	found, complete, seen, err := offsetPages[sprintJSON](ctx, c, path, v, sprintPage, q.Limit)
	out := SprintList{Sprints: make([]Sprint, 0, len(found)), Complete: complete, Read: seen.date, mark: seen.mark}
	for _, j := range found {
		out.Sprints = append(out.Sprints, j.sprint())
	}
	if err != nil {
		out.Complete = false
		return out, fmt.Errorf("jira: sprints of board %d: %w", q.Board, err)
	}
	return out, nil
}

// sprintPage is the most sprints a listing page asks for: the spec's
// default, the most it documents. Asking for more is not measured.
const sprintPage = 50

// BoardSprints returns every sprint of a board, as Sprints with no
// filter lists them, from the client's cache once read. The listing is
// kept with the board's configuration and dropped with it by
// ForgetBoard; a failed or partial listing is not kept. Read says how old
// a cached listing is. The value is the caller's own.
func (c *Client) BoardSprints(ctx context.Context, board int64) (SprintList, error) {
	if err := c.admit(ctx); err != nil {
		return SprintList{}, err
	}
	if err := checkAgileID("board", board); err != nil {
		return SprintList{}, err
	}
	if l, ok := c.sprintLists.get(ctx, board); ok {
		return l, nil
	}
	epoch := c.sprintLists.epoch()
	l, err := c.sprints(ctx, SprintQuery{Board: board})
	if err != nil {
		return l, err
	}
	c.sprintLists.put(epoch, l.mark, board, l)
	return l, nil
}

func (c *Client) sprint(ctx context.Context, id int64) (Sprint, error) {
	path, err := sprintPath(id)
	if err != nil {
		return Sprint{}, err
	}
	var j sprintJSON
	if _, err := c.call(ctx, trackerapi.Request{Path: path}, &j); err != nil {
		return Sprint{}, fmt.Errorf("jira: sprint %d: %w", id, err)
	}
	return j.sprint(), nil
}

// sprintOr is known when it is sprint id as the caller read it, and
// sprint id read now when known is nil. A known sprint of another ID is
// refused: the checks made on it would be of the wrong record.
func (c *Client) sprintOr(ctx context.Context, id int64, known *Sprint) (Sprint, error) {
	switch {
	case known == nil:
		return c.sprint(ctx, id)
	case known.ID != id:
		return Sprint{}, notSent("sprint %d: the sprint given as read is sprint %d", id, known.ID)
	}
	return *known, nil
}

// openSprintNamed resolves name among the active and future sprints of
// the scrum boards of project: the sprints an issue can be put in. A
// sprint several boards share counts once. Like every name match, a name
// that matches several sprints or none is a *MatchError listing them.
func (c *Client) openSprintNamed(ctx context.Context, project, name string) (Sprint, error) {
	boards, err := c.boards(ctx, BoardQuery{Project: project, Types: []BoardType{BoardScrum}})
	if err != nil {
		return Sprint{}, err
	}
	var open []Sprint
	seen := map[int64]bool{}
	for _, b := range boards.Boards {
		list, err := c.sprints(ctx, SprintQuery{Board: b.ID, States: []SprintState{SprintActive, SprintFuture}})
		if err != nil {
			return Sprint{}, err
		}
		for _, s := range list.Sprints {
			if !seen[s.ID] {
				seen[s.ID] = true
				open = append(open, s)
			}
		}
	}
	cands := make([]Candidate, len(open))
	for i, s := range open {
		cands[i] = Candidate{ID: strconv.FormatInt(s.ID, 10), Name: s.Name}
	}
	hit, err := match("open sprint on the boards of "+project, name, cands)
	if err != nil {
		return Sprint{}, err
	}
	for _, s := range open {
		if strconv.FormatInt(s.ID, 10) == hit.ID {
			return s, nil
		}
	}
	return Sprint{}, fmt.Errorf("jira: sprint %s vanished from its own list", hit.ID)
}

// writeSprint sends a sprint write and decodes the sprint Jira stored:
// a create (id zero; documented: 201 naming the new sprint) or a change
// of sprint id (200 naming that sprint). A failure is a StepSprint
// *StepError; an Applied one returns what of the sprint decoded, its ID
// at least, and any other returns the zero Sprint.
func (c *Client) writeSprint(ctx context.Context, method, path string, id int64, body any) (Sprint, error) {
	want := bodyAnswer(namesID, http.StatusCreated)
	if id > 0 {
		want = bodyAnswer(namesThisID(strconv.FormatInt(id, 10)), http.StatusOK)
	}
	var j sprintJSON
	w, err := c.write(ctx, trackerapi.Request{Method: method, Path: path, Body: body}, want, &j)
	s := j.sprint()
	s.ID, _ = strconv.ParseInt(w.id(), 10, 64)
	if err != nil {
		return s, &StepError{Step: StepSprint, Err: err}
	}
	return s, nil
}

// SprintInput is a new sprint. Start and End are optional; zero leaves
// them unset, and a sprint needs both before it starts.
type SprintInput struct {
	Board int64
	Name  string
	Goal  string
	Start time.Time
	End   time.Time
}

// CreateSprint creates a future sprint on a board. Jira trims the name.
func (c *Client) CreateSprint(ctx context.Context, in SprintInput) (Sprint, error) {
	if err := c.admit(ctx); err != nil {
		return Sprint{}, err
	}
	if err := checkAgileID("board", in.Board); err != nil {
		return Sprint{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return Sprint{}, notSent("create sprint: no name")
	}
	if err := checkSprintDates(in.Start, in.End); err != nil {
		return Sprint{}, err
	}
	body := map[string]any{"name": in.Name, "originBoardId": in.Board}
	if in.Goal != "" {
		body["goal"] = in.Goal
	}
	putDates(body, in.Start, in.End)
	s, err := c.writeSprint(ctx, http.MethodPost, agilePath+"sprint", 0, body)
	if err != nil {
		return s, fmt.Errorf("jira: create sprint %q: %w", in.Name, err)
	}
	return s, nil
}

func checkSprintDates(start, end time.Time) error {
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return notSent("a sprint's end %s is not after its start %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	return nil
}

func putDates(body map[string]any, start, end time.Time) {
	if !start.IsZero() {
		body["startDate"] = start.Format(sprintTimeLayout)
	}
	if !end.IsZero() {
		body["endDate"] = end.Format(sprintTimeLayout)
	}
}

// SprintUpdate changes the fields of a sprint that are set; nil leaves a
// field as it is.
type SprintUpdate struct {
	ID    int64
	Name  *string
	Goal  *string
	Start *time.Time
	End   *time.Time
	// Current is the sprint as the caller read it just now (a board's
	// sprint listing holds each sprint whole); nil reads it.
	Current *Sprint
}

// UpdateSprint changes a sprint's name, goal or dates. The sprint is read
// first, or taken from Current: a closed sprint's dates are refused,
// since Jira ignores them, and the dates the sprint ends up with must put
// its end after its start. StartSprint and CompleteSprint change its
// state.
func (c *Client) UpdateSprint(ctx context.Context, in SprintUpdate) (Sprint, error) {
	if err := c.admit(ctx); err != nil {
		return Sprint{}, err
	}
	path, err := sprintPath(in.ID)
	if err != nil {
		return Sprint{}, err
	}
	body := map[string]any{}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return Sprint{}, notSent("update sprint %d: an empty name", in.ID)
		}
		body["name"] = *in.Name
	}
	if in.Goal != nil {
		body["goal"] = *in.Goal
	}
	if in.Name == nil && in.Goal == nil && in.Start == nil && in.End == nil {
		return Sprint{}, notSent("update sprint %d: nothing to change", in.ID)
	}
	cur, err := c.sprintOr(ctx, in.ID, in.Current)
	if err != nil {
		return Sprint{}, notSentErr(err)
	}
	start, end := cur.Start, cur.End
	if in.Start != nil {
		start = *in.Start
	}
	if in.End != nil {
		end = *in.End
	}
	if cur.State == SprintClosed && (in.Start != nil || in.End != nil) {
		return Sprint{}, notSent("sprint %d is closed: only its name and goal change", in.ID)
	}
	if err := checkSprintDates(start, end); err != nil {
		return Sprint{}, err
	}
	if in.Start != nil {
		putDates(body, start, time.Time{})
	}
	if in.End != nil {
		putDates(body, time.Time{}, end)
	}
	s, err := c.writeSprint(ctx, http.MethodPost, path, in.ID, body)
	if err != nil {
		return s, fmt.Errorf("jira: update sprint %d: %w", in.ID, err)
	}
	return s, nil
}

// SprintStart starts a future sprint. A zero Start or End keeps the date
// the sprint has; it needs both. Current is as for SprintUpdate.
type SprintStart struct {
	ID      int64
	Start   time.Time
	End     time.Time
	Current *Sprint
}

// StartSprint makes a future sprint active with its start and end dates.
// It is refused before anything is sent when the sprint is not future or
// would lack a date.
func (c *Client) StartSprint(ctx context.Context, in SprintStart) (Sprint, error) {
	if err := c.admit(ctx); err != nil {
		return Sprint{}, err
	}
	path, err := sprintPath(in.ID)
	if err != nil {
		return Sprint{}, err
	}
	cur, err := c.sprintOr(ctx, in.ID, in.Current)
	if err != nil {
		return Sprint{}, notSentErr(err)
	}
	if cur.State != SprintFuture {
		return Sprint{}, notSent("sprint %d is %s; only a future sprint starts", in.ID, cur.State)
	}
	start, end := cur.Start, cur.End
	if !in.Start.IsZero() {
		start = in.Start
	}
	if !in.End.IsZero() {
		end = in.End
	}
	if start.IsZero() || end.IsZero() {
		return Sprint{}, notSent("sprint %d needs a start and an end date to start", in.ID)
	}
	if err := checkSprintDates(start, end); err != nil {
		return Sprint{}, err
	}
	body := map[string]any{"state": SprintActive}
	putDates(body, start, end)
	s, err := c.writeSprint(ctx, http.MethodPost, path, in.ID, body)
	if err != nil {
		return s, fmt.Errorf("jira: start sprint %d: %w", in.ID, err)
	}
	return s, nil
}

// DeleteSprint deletes a sprint. Jira moves its open issues to the
// backlog.
func (c *Client) DeleteSprint(ctx context.Context, id int64) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := sprintPath(id)
	if err != nil {
		return err
	}
	// Documented: 204.
	if _, err := c.write(ctx, trackerapi.Request{Method: http.MethodDelete, Path: path}, emptyAnswer(http.StatusNoContent), nil); err != nil {
		return fmt.Errorf("jira: delete sprint %d: %w", id, err)
	}
	return nil
}
