package jira

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// agilePath and softwarePath are the Jira Software API roots: boards,
// sprints, ranking and estimation under agilePath, the token-paged issue
// lists under softwarePath, which replaced agilePath's lists.
const (
	agilePath    = "rest/agile/1.0/"
	softwarePath = "rest/software/1.0/"
)

// boardConfigCacheSize bounds the board configurations a Client keeps.
const boardConfigCacheSize = 64

// BoardType is a board's kind. A value outside the constants below is
// refused as a filter before anything is sent; a board read from Jira
// carries whatever type Jira names.
type BoardType string

const (
	BoardScrum  BoardType = "scrum"
	BoardKanban BoardType = "kanban"
	BoardSimple BoardType = "simple"
)

func (t BoardType) valid() bool {
	return t == BoardScrum || t == BoardKanban || t == BoardSimple
}

// Board is a Jira Software board.
type Board struct {
	ID      int64
	Name    string
	Type    BoardType
	Private bool
	// Location is the project or user the board belongs to; zero when
	// Jira does not say.
	Location BoardLocation
}

// BoardLocation is the container a board is in: a project (ProjectKey
// set) or a user (UserAccountID set).
type BoardLocation struct {
	ProjectID      int64
	ProjectKey     string
	ProjectName    string
	ProjectTypeKey string
	UserAccountID  string
	Name           string
}

type boardJSON struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Type     BoardType `json:"type"`
	Private  bool      `json:"isPrivate"`
	Location struct {
		ProjectID      int64  `json:"projectId"`
		ProjectKey     string `json:"projectKey"`
		ProjectName    string `json:"projectName"`
		ProjectTypeKey string `json:"projectTypeKey"`
		UserAccountID  string `json:"userAccountId"`
		Name           string `json:"name"`
	} `json:"location"`
}

func (b boardJSON) board() Board {
	return Board{ID: b.ID, Name: b.Name, Type: b.Type, Private: b.Private, Location: BoardLocation(b.Location)}
}

// BoardQuery filters the boards Boards lists.
type BoardQuery struct {
	// Project is a project key or ID; empty lists every board. Jira lists
	// the boards whose filter names the project.
	Project string
	// Types keeps the boards of these types; none keeps every type.
	Types []BoardType
	// Name keeps the boards whose name holds it.
	Name string
	// Limit stops after that many boards; zero reads every page.
	Limit int
}

// BoardList is the boards a listing returned. Complete is false when the
// limit cut the listing.
type BoardList struct {
	Boards   []Board
	Complete bool
}

// Boards lists the boards the user can see, page by page to the end or
// q.Limit. A failure after some pages returns them with the error.
func (c *Client) Boards(ctx context.Context, q BoardQuery) (BoardList, error) {
	if err := c.admit(ctx); err != nil {
		return BoardList{}, err
	}
	return c.boards(ctx, q)
}

func (c *Client) boards(ctx context.Context, q BoardQuery) (BoardList, error) {
	v := url.Values{}
	if q.Project != "" {
		if err := checkProject(q.Project); err != nil {
			return BoardList{}, err
		}
		v.Set("projectKeyOrId", q.Project)
	}
	if q.Limit < 0 {
		return BoardList{}, notSent("list boards: a negative limit")
	}
	var types []string
	for _, t := range q.Types {
		if !t.valid() {
			return BoardList{}, notSent("list boards: %q is not a BoardType", t)
		}
		types = append(types, string(t))
	}
	if len(types) > 0 {
		v.Set("type", strings.Join(types, ","))
	}
	if q.Name != "" {
		v.Set("name", q.Name)
	}
	found, complete, _, err := offsetPages[boardJSON](ctx, c, agilePath+"board", v, 50, q.Limit)
	out := BoardList{Boards: make([]Board, len(found)), Complete: complete}
	for i, b := range found {
		out.Boards[i] = b.board()
	}
	if err != nil {
		return out, fmt.Errorf("jira: list boards: %w", err)
	}
	return out, nil
}

// Board returns one board.
func (c *Client) Board(ctx context.Context, id int64) (Board, error) {
	if err := c.admit(ctx); err != nil {
		return Board{}, err
	}
	if err := checkAgileID("board", id); err != nil {
		return Board{}, err
	}
	var b boardJSON
	if _, err := c.call(ctx, trackerapi.Request{Path: agilePath + "board/" + strconv.FormatInt(id, 10)}, &b); err != nil {
		return Board{}, fmt.Errorf("jira: board %d: %w", id, err)
	}
	return b.board(), nil
}

// checkAgileID refuses an ID that is not positive; what names it.
func checkAgileID(what string, id int64) error {
	if id <= 0 {
		return notSent("%d is not a %s ID", id, what)
	}
	return nil
}

// ColumnConstraint is what a board's column limits count.
type ColumnConstraint string

const (
	ConstraintNone           ColumnConstraint = "none"
	ConstraintIssueCount     ColumnConstraint = "issueCount"
	ConstraintIssueCountSubs ColumnConstraint = "issueCountExclSubs"
)

// BoardColumn is a board column and the statuses mapped to it. Min and
// Max are its limits, zero when it has none.
type BoardColumn struct {
	Name      string
	StatusIDs []string
	Min       int
	Max       int
}

// EstimationType is how a board estimates its issues.
type EstimationType string

const (
	EstimateNone       EstimationType = "none"
	EstimateIssueCount EstimationType = "issueCount"
	EstimateField      EstimationType = "field"
)

// Estimation is a board's estimation: by a field (FieldID set), by issue
// count, or none. Jira gives only a scrum board's.
type Estimation struct {
	Type      EstimationType
	FieldID   string
	FieldName string
	// Custom reports a custom field; the original time estimate
	// (timeoriginalestimate) is the system field a board can use.
	Custom bool
}

// LocationType is what a board is located in.
type LocationType string

const (
	LocatedInProject LocationType = "project"
	LocatedOnUser    LocationType = "user"
)

// ConfigLocation is where a board's configuration says the board is: a
// project (Type LocatedInProject, ID the project's ID) or a user (Type
// LocatedOnUser, or any other type Jira names, whose ID is not a
// project's). Measured on Jira Cloud: the configuration's location has
// id (a string), key, name, self and type (docs/references/
// tracker-api-measurements.md); the spec documents type and
// projectKeyOrId.
type ConfigLocation struct {
	Type LocationType
	ID   string
	Key  string
	Name string
}

// ProjectID is the ID of the project the board is located in; empty for
// a board located anywhere else.
func (l ConfigLocation) ProjectID() string {
	if l.Type != LocatedInProject {
		return ""
	}
	return l.ID
}

// BoardConfig is a board's configuration.
type BoardConfig struct {
	BoardID int64
	Name    string
	Type    BoardType
	// Location is where the board is; zero when Jira does not say.
	Location ConfigLocation
	// FilterID is the saved filter that selects the board's issues.
	FilterID string
	// SubQuery is a kanban board's JQL sub-query; empty elsewhere.
	SubQuery   string
	Columns    []BoardColumn // in board order
	Constraint ColumnConstraint
	Estimation Estimation
	// RankFieldID is the number of the custom field the board ranks by
	// (customfield_<n>); zero when Jira does not name one.
	RankFieldID int64
}

// DoneStatusIDs are the statuses of the board's done column: the last
// column with statuses mapped to it. None when no column has any.
func (b BoardConfig) DoneStatusIDs() []string {
	for i := len(b.Columns) - 1; i >= 0; i-- {
		if len(b.Columns[i].StatusIDs) > 0 {
			return slices.Clone(b.Columns[i].StatusIDs)
		}
	}
	return nil
}

func cloneBoardConfig(b BoardConfig) BoardConfig {
	b.Columns = slices.Clone(b.Columns)
	for i := range b.Columns {
		b.Columns[i].StatusIDs = slices.Clone(b.Columns[i].StatusIDs)
	}
	return b
}

type boardConfigJSON struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Type     BoardType `json:"type"`
	Location struct {
		Type LocationType `json:"type"`
		ID   string       `json:"id"`
		Key  string       `json:"key"`
		Name string       `json:"name"`
	} `json:"location"`
	Filter struct {
		ID string `json:"id"`
	} `json:"filter"`
	SubQuery struct {
		Query string `json:"query"`
	} `json:"subQuery"`
	ColumnConfig struct {
		Columns []struct {
			Name     string `json:"name"`
			Min      int    `json:"min"`
			Max      int    `json:"max"`
			Statuses []struct {
				ID string `json:"id"`
			} `json:"statuses"`
		} `json:"columns"`
		Constraint ColumnConstraint `json:"constraintType"`
	} `json:"columnConfig"`
	Estimation struct {
		Type  EstimationType `json:"type"`
		Field struct {
			FieldID     string `json:"fieldId"`
			DisplayName string `json:"displayName"`
		} `json:"field"`
	} `json:"estimation"`
	Ranking struct {
		RankCustomFieldID int64 `json:"rankCustomFieldId"`
	} `json:"ranking"`
}

func (j boardConfigJSON) config() BoardConfig {
	out := BoardConfig{
		BoardID: j.ID, Name: j.Name, Type: j.Type, FilterID: j.Filter.ID, SubQuery: j.SubQuery.Query,
		Constraint: j.ColumnConfig.Constraint, RankFieldID: j.Ranking.RankCustomFieldID,
		Estimation: Estimation{Type: j.Estimation.Type, FieldID: j.Estimation.Field.FieldID, FieldName: j.Estimation.Field.DisplayName},
		Location:   ConfigLocation{Type: j.Location.Type, ID: j.Location.ID, Key: j.Location.Key, Name: j.Location.Name},
	}
	out.Estimation.Custom = strings.HasPrefix(out.Estimation.FieldID, "customfield_")
	for _, col := range j.ColumnConfig.Columns {
		bc := BoardColumn{Name: col.Name, Min: col.Min, Max: col.Max}
		for _, s := range col.Statuses {
			bc.StatusIDs = append(bc.StatusIDs, s.ID)
		}
		out.Columns = append(out.Columns, bc)
	}
	return out
}

// BoardConfig returns a board's configuration, from the client's cache
// once read; ForgetBoard drops it. The value is the caller's own.
func (c *Client) BoardConfig(ctx context.Context, board int64) (BoardConfig, error) {
	if err := c.admit(ctx); err != nil {
		return BoardConfig{}, err
	}
	return c.boardConfig(ctx, board)
}

func (c *Client) boardConfig(ctx context.Context, board int64) (BoardConfig, error) {
	if err := checkAgileID("board", board); err != nil {
		return BoardConfig{}, err
	}
	if cfg, ok := c.configs.get(board); ok {
		return cfg, nil
	}
	epoch := c.configs.epoch()
	var j boardConfigJSON
	path := agilePath + "board/" + strconv.FormatInt(board, 10) + "/configuration"
	if _, err := c.call(ctx, trackerapi.Request{Path: path}, &j); err != nil {
		return BoardConfig{}, fmt.Errorf("jira: configuration of board %d: %w", board, err)
	}
	cfg := j.config()
	c.configs.put(epoch, board, cfg)
	return cfg, nil
}

// ForgetBoard drops what the client holds of one board, its
// configuration and its sprint listing, so the next read asks Jira; the
// other boards' stay. A read in flight stores nothing.
func (c *Client) ForgetBoard(board int64) {
	c.configs.drop(board)
	c.sprintLists.drop(board)
}
