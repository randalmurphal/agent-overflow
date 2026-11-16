package linear

import (
	"context"
	"errors"
	"fmt"
)

// A write returns what Linear stored. When it fails, WriteNotApplied
// tells a refusal from an unknown outcome; after an unknown outcome the
// caller reads the issue before offering the write again. A caller marks
// a person's own write with trackerapi.WithInteractive.

// CreateIssueInput is a new issue. Unset fields take Linear's defaults:
// the team's initial status, the template's assignee, no estimate. Null
// is accepted where noted.
type CreateIssueInput struct {
	TeamID      string // required
	Title       string // required
	Description string // Markdown; empty for none
	StateID     Optional[string]
	AssigneeID  Optional[string] // Null leaves the issue unassigned
	Priority    Optional[Priority]
	// Estimate is in the team's scale, 0 to 64, mapped by Linear to the
	// nearest value; Null leaves the issue unestimated.
	Estimate  Optional[int]
	LabelIDs  []string
	CycleID   Optional[string]
	ProjectID Optional[string] // Null keeps a sub-issue out of its parent's project
	ParentID  Optional[string] // an issue ID or identifier
}

// issueInput is the wire form of IssueCreateInput and IssueUpdateInput;
// each call fills the fields its input allows.
type issueInput struct {
	TeamID            Optional[string]   `json:"teamId,omitzero"`
	Title             Optional[string]   `json:"title,omitzero"`
	Description       Optional[string]   `json:"description,omitzero"`
	StateID           Optional[string]   `json:"stateId,omitzero"`
	AssigneeID        Optional[string]   `json:"assigneeId,omitzero"`
	Priority          Optional[Priority] `json:"priority,omitzero"`
	Estimate          Optional[int]      `json:"estimate,omitzero"`
	LabelIDs          Optional[[]string] `json:"labelIds,omitzero"`
	AddedLabelIDs     []string           `json:"addedLabelIds,omitempty"`
	RemovedLabelIDs   []string           `json:"removedLabelIds,omitempty"`
	CycleID           Optional[string]   `json:"cycleId,omitzero"`
	ProjectID         Optional[string]   `json:"projectId,omitzero"`
	ParentID          Optional[string]   `json:"parentId,omitzero"`
	SortOrder         Optional[float64]  `json:"sortOrder,omitzero"`
	PrioritySortOrder Optional[float64]  `json:"prioritySortOrder,omitzero"`
	SubIssueSortOrder Optional[float64]  `json:"subIssueSortOrder,omitzero"`
}

var opCreateIssue = newMutation("CreateIssue", `mutation CreateIssue($input: IssueCreateInput!) {
  result: issueCreate(input: $input) { success value: issue { ...IssueFields } }
}
`+issueFields)

type inputVars struct {
	Input any `json:"input"`
}

// CreateIssue creates an issue and returns it with its identifier.
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (*Issue, error) {
	wire, err := in.wire()
	if err != nil {
		return nil, invalid(opCreateIssue.name, "%w", err)
	}
	return mutateStored(ctx, c, opCreateIssue, inputVars{wire}, wireIssue.typed)
}

func (in CreateIssueInput) wire() (issueInput, error) {
	switch {
	case in.TeamID == "":
		return issueInput{}, errors.New("a team ID is required")
	case in.Title == "":
		return issueInput{}, errors.New("a title is required")
	}
	if err := checkIDs("LabelIDs", in.LabelIDs); err != nil {
		return issueInput{}, err
	}
	for _, f := range []field{
		{"StateID", in.StateID, false}, {"AssigneeID", in.AssigneeID, true}, {"CycleID", in.CycleID, false},
		{"ProjectID", in.ProjectID, true}, {"ParentID", in.ParentID, false},
	} {
		if err := f.check(); err != nil {
			return issueInput{}, err
		}
	}
	if in.Priority.IsNull() {
		return issueInput{}, errors.New("the priority cannot be null; set PriorityNone")
	}
	w := issueInput{
		TeamID: Set(in.TeamID), Title: Set(in.Title), StateID: in.StateID, AssigneeID: in.AssigneeID,
		Priority: in.Priority, Estimate: in.Estimate, CycleID: in.CycleID, ProjectID: in.ProjectID, ParentID: in.ParentID,
	}
	if in.Description != "" {
		w.Description = Set(in.Description)
	}
	if len(in.LabelIDs) > 0 {
		w.LabelIDs = Set(in.LabelIDs)
	}
	return w, nil
}

// field is an optional ID input to check: Set holds a non-empty ID, and
// Null is accepted only where the field can be cleared.
type field struct {
	name     string
	value    Optional[string]
	nullable bool
}

func (f field) check() error {
	switch {
	case f.value.IsSet() && f.value.Value() == "":
		return fmt.Errorf("%s is set to an empty ID; use Null to clear it", f.name)
	case f.value.IsNull() && !f.nullable:
		return fmt.Errorf("%s cannot be cleared", f.name)
	}
	return nil
}

// UpdateIssueInput changes an issue. An unset field is left alone, Set
// changes it and Null clears it where Linear allows.
type UpdateIssueInput struct {
	Title Optional[string] // cannot be cleared
	// Description replaces the whole Markdown description; Null clears
	// it (sent as the empty string, which is how Linear clears it).
	Description Optional[string]
	StateID     Optional[string] // cannot be cleared
	AssigneeID  Optional[string]
	Priority    Optional[Priority] // cannot be cleared; set PriorityNone
	Estimate    Optional[int]
	// LabelIDs replaces the labels; Null removes them all. AddedLabelIDs
	// and RemovedLabelIDs change some without naming the rest and cannot
	// be combined with LabelIDs.
	LabelIDs        Optional[[]string]
	AddedLabelIDs   []string
	RemovedLabelIDs []string
	// CycleID moves the issue into a cycle of its team; Null takes it out,
	// subject to the team's automatic cycle assignment.
	CycleID   Optional[string]
	ProjectID Optional[string]
	ParentID  Optional[string] // an issue ID or identifier; Null makes it top-level
	// TeamID moves the issue to another team, which gives it a new
	// identifier; cannot be cleared.
	TeamID Optional[string]
}

var opUpdateIssue = newMutation("UpdateIssue", `mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
  result: issueUpdate(id: $id, input: $input) { success value: issue { ...IssueFields } }
}
`+issueFields)

type updateVars struct {
	ID    string `json:"id"`
	Input any    `json:"input"`
}

// UpdateIssue changes the issue id (an ID or identifier) and returns it.
func (c *Client) UpdateIssue(ctx context.Context, id string, in UpdateIssueInput) (*Issue, error) {
	wire, err := in.wire()
	if err == nil && id == "" {
		err = errors.New("an issue ID is required")
	}
	if err != nil {
		return nil, invalid(opUpdateIssue.name, "%w", err)
	}
	return c.updateIssue(ctx, id, wire)
}

func (c *Client) updateIssue(ctx context.Context, id string, wire issueInput) (*Issue, error) {
	return mutateStored(ctx, c, opUpdateIssue, updateVars{id, wire}, wireIssue.typed)
}

func (in UpdateIssueInput) wire() (issueInput, error) {
	for _, f := range []field{
		{"StateID", in.StateID, false}, {"AssigneeID", in.AssigneeID, true}, {"CycleID", in.CycleID, true},
		{"ProjectID", in.ProjectID, true}, {"ParentID", in.ParentID, true}, {"TeamID", in.TeamID, false},
	} {
		if err := f.check(); err != nil {
			return issueInput{}, err
		}
	}
	switch {
	case in.Title.IsNull() || (in.Title.IsSet() && in.Title.Value() == ""):
		return issueInput{}, errors.New("the title cannot be cleared")
	case in.Priority.IsNull():
		return issueInput{}, errors.New("the priority cannot be null; set PriorityNone")
	case !in.LabelIDs.IsZero() && (len(in.AddedLabelIDs) > 0 || len(in.RemovedLabelIDs) > 0):
		return issueInput{}, errors.New("LabelIDs cannot be combined with AddedLabelIDs or RemovedLabelIDs")
	}
	for _, list := range []struct {
		name string
		ids  []string
	}{{"LabelIDs", in.LabelIDs.Value()}, {"AddedLabelIDs", in.AddedLabelIDs}, {"RemovedLabelIDs", in.RemovedLabelIDs}} {
		if err := checkIDs(list.name, list.ids); err != nil {
			return issueInput{}, err
		}
	}
	w := issueInput{
		Title: in.Title, Description: in.Description, StateID: in.StateID, AssigneeID: in.AssigneeID,
		Priority: in.Priority, Estimate: in.Estimate, LabelIDs: in.LabelIDs, AddedLabelIDs: in.AddedLabelIDs,
		RemovedLabelIDs: in.RemovedLabelIDs, CycleID: in.CycleID, ProjectID: in.ProjectID, ParentID: in.ParentID,
		TeamID: in.TeamID,
	}
	if in.Description.IsNull() {
		w.Description = Set("")
	}
	if in.LabelIDs.IsNull() {
		w.LabelIDs = Set([]string{})
	}
	if w.empty() {
		return issueInput{}, errors.New("nothing to change")
	}
	return w, nil
}

func (w issueInput) empty() bool {
	return w.TeamID.IsZero() && w.Title.IsZero() && w.Description.IsZero() && w.StateID.IsZero() &&
		w.AssigneeID.IsZero() && w.Priority.IsZero() && w.Estimate.IsZero() && w.LabelIDs.IsZero() &&
		len(w.AddedLabelIDs) == 0 && len(w.RemovedLabelIDs) == 0 && w.CycleID.IsZero() &&
		w.ProjectID.IsZero() && w.ParentID.IsZero() && w.SortOrder.IsZero() &&
		w.PrioritySortOrder.IsZero() && w.SubIssueSortOrder.IsZero()
}

var opArchiveIssue = newMutation("ArchiveIssue", `mutation ArchiveIssue($id: String!) {
  result: issueArchive(id: $id) { success value: entity { ...IssueFields } }
}
`+issueFields)

// ArchiveIssue archives an issue and its sub-issues; UnarchiveIssue
// restores it.
func (c *Client) ArchiveIssue(ctx context.Context, id string) (*Issue, error) {
	return c.issueByID(ctx, opArchiveIssue, id)
}

var opUnarchiveIssue = newMutation("UnarchiveIssue", `mutation UnarchiveIssue($id: String!) {
  result: issueUnarchive(id: $id) { success value: entity { ...IssueFields } }
}
`+issueFields)

// UnarchiveIssue restores an archived or trashed issue.
func (c *Client) UnarchiveIssue(ctx context.Context, id string) (*Issue, error) {
	return c.issueByID(ctx, opUnarchiveIssue, id)
}

func (c *Client) issueByID(ctx context.Context, op operation, id string) (*Issue, error) {
	if id == "" {
		return nil, invalid(op.name, "an issue ID is required")
	}
	return mutateStored(ctx, c, op, idVars{id}, wireIssue.typed)
}

var opDeleteIssue = newMutation("DeleteIssue", `mutation DeleteIssue($id: String!, $permanently: Boolean) {
  result: issueDelete(id: $id, permanentlyDelete: $permanently) { success }
}`)

type deleteIssueVars struct {
	ID          string `json:"id"`
	Permanently bool   `json:"permanently,omitempty"`
}

// DeleteIssue moves an issue and its sub-issues to the trash, from which
// UnarchiveIssue restores it for 30 days. With permanently it is deleted
// at once, which needs the workspace permission to manage entities, and
// polling can no longer see it.
func (c *Client) DeleteIssue(ctx context.Context, id string, permanently bool) error {
	if id == "" {
		return invalid(opDeleteIssue.name, "an issue ID is required")
	}
	_, err := mutate[struct{}](ctx, c, opDeleteIssue, deleteIssueVars{id, permanently})
	return err
}
