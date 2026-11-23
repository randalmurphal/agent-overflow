package linear

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Board is what a team's board is built from: the team, its workflow
// states, the cycles a window keeps and every workspace user,
// deactivated ones included.
type Board struct {
	Team   Team
	States []WorkflowState
	Cycles []Cycle
	Users  []User
}

// opTeamBoard selects the first page of each list with the page size and
// variables of the read that continues it (WorkflowStates, Cycles,
// Users), so it costs what those first pages and Team cost apart, less
// the three requests it saves. The team is a String! argument and the
// filters' IDs are ID!, so the team's ID is sent as both.
var opTeamBoard = newQuery("TeamBoard", "team", `query TeamBoard($id: String!, $teamId: ID!, $first: Int!, $cycles: CycleFilter!, $cycleFirst: Int!) {
  team(id: $id) { ...TeamFields }
  states: workflowStates(filter: { team: { id: { eq: $teamId } } }, first: $first) {
    nodes { ...StateFields } pageInfo { hasNextPage endCursor }
  }
  cycles(filter: $cycles, first: $cycleFirst) { nodes { ...CycleFields } pageInfo { hasNextPage endCursor } }
  users(includeDisabled: true, first: $first) { nodes { ...UserFields } pageInfo { hasNextPage endCursor } }
}
`+teamFields+stateFields+cycleFields+userFields)

type teamBoardVars struct {
	ID         string      `json:"id"`
	TeamID     string      `json:"teamId"`
	First      int         `json:"first"`
	Cycles     cycleFilter `json:"cycles"`
	CycleFirst int         `json:"cycleFirst"`
}

type teamBoardData struct {
	Team   *wireTeam              `json:"team"`
	States *connection[wireState] `json:"states"`
	Cycles *connection[wireCycle] `json:"cycles"`
	Users  *connection[User]      `json:"users"`
}

// TeamBoard reads team teamID's board in one request: the team, its
// states (WorkflowStates), its cycles that are active or future or that
// ended or completed at or after keptSince, every cycle when it is zero
// (Cycles), and the workspace's users (Users with includeDisabled). A
// list longer than its first page continues with its own read, from the
// page's cursor, all as one trackerapi.Walk. A team the key cannot see
// matches trackerapi.ErrNotFound.
func (c *Client) TeamBoard(ctx context.Context, teamID string, keptSince time.Time) (*Board, error) {
	cycles, err := CycleQuery{TeamID: teamID, KeptSince: keptSince}.vars(opTeamBoard.name)
	if err != nil {
		return nil, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (*Board, error) {
		var data teamBoardData
		_, err := c.call(ctx, opTeamBoard, teamBoardVars{teamID, teamID, catalogPage, cycles.Filter, cycles.First}, &data)
		if err != nil {
			return nil, err
		}
		if data.Team == nil || data.States == nil || data.Cycles == nil || data.Users == nil {
			return nil, fmt.Errorf("linear %s: the answer lacks the team or one of its lists", opTeamBoard.name)
		}
		b := &Board{Team: data.Team.typed()}
		if b.States, err = continued(ctx, c, opWorkflowStates, statesPage(teamID), wireState.typed, *data.States); err != nil {
			return nil, err
		}
		if b.Cycles, err = continued(ctx, c, opCycles, cycles.after, wireCycle.typed, *data.Cycles); err != nil {
			return nil, err
		}
		if b.Users, err = continued(ctx, c, opUsers, usersPage(true), same[User], *data.Users); err != nil {
			return nil, err
		}
		return b, nil
	})
}

// continued is first, a list of opTeamBoard's answer, and the pages of op
// after it.
func continued[W, T any](ctx context.Context, c *Client, op operation, varsFor func(after string) any, typed func(W) T, first connection[W]) ([]T, error) {
	p, err := page(opTeamBoard, first, Usage{}, typed)
	if err != nil {
		return nil, err
	}
	return rest(ctx, c, op, varsFor, typed, p)
}
