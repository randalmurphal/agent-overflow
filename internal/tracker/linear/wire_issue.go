package linear

import "time"

// issueSelection is what every read of an issue selects. Linear charges
// a page of issues by the issues it returns, about 3 points each
// (measured). Related entities are selected by ID, and their names are
// read by ID for the IDs a result names, with two exceptions nested in
// the issue because they cost less than a second request. The team's
// estimate scale is a scalar on an object already selected, so a list of
// rows tells whether their estimates count without reading the team. The
// labels' names cost about 1.5 points an issue whatever the nested page
// size or the labels it holds (measured), so the page is maxIssuePage:
// only an issue with more labels than that, or with one the list leaves
// out, needs LabelsByID.
// labelIds stays the complete set.
const issueSelection = `
  id identifier number title description url branchName
  priority estimate sortOrder prioritySortOrder subIssueSortOrder
  team { id issueEstimationType } state { id } assignee { id } creator { id } cycle { id } project { id } parent { id }
  labelIds labels(first: 250) { nodes { id name } }
  dueDate createdAt updatedAt startedAt completedAt canceledAt archivedAt trashed
`

const issueFields = `fragment IssueFields on Issue {` + issueSelection + `}`

// issueSearchFields is issueSelection on a search result, a type of its
// own with the same fields, which wireIssue decodes too.
const issueSearchFields = `fragment IssueSearchFields on IssueSearchResult {` + issueSelection + `}`

type wireIssue struct {
	ID                string     `json:"id"`
	Identifier        string     `json:"identifier"`
	Number            float64    `json:"number"`
	Title             string     `json:"title"`
	Description       string     `json:"description"`
	URL               string     `json:"url"`
	BranchName        string     `json:"branchName"`
	Priority          float64    `json:"priority"`
	Estimate          *float64   `json:"estimate"`
	SortOrder         float64    `json:"sortOrder"`
	PrioritySortOrder float64    `json:"prioritySortOrder"`
	SubIssueSortOrder *float64   `json:"subIssueSortOrder"`
	Team              *issueTeam `json:"team"`
	State             *ref       `json:"state"`
	Assignee          *ref       `json:"assignee"`
	Creator           *ref       `json:"creator"`
	Cycle             *ref       `json:"cycle"`
	Project           *ref       `json:"project"`
	Parent            *ref       `json:"parent"`
	LabelIDs          []string   `json:"labelIds"`
	Labels            labelRefs  `json:"labels"`
	DueDate           Date       `json:"dueDate"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	StartedAt         *time.Time `json:"startedAt"`
	CompletedAt       *time.Time `json:"completedAt"`
	CanceledAt        *time.Time `json:"canceledAt"`
	ArchivedAt        *time.Time `json:"archivedAt"`
	Trashed           *bool      `json:"trashed"`
}

// labelRefs is an issue's labels as issueSelection nests them.
type labelRefs struct {
	Nodes []LabelRef `json:"nodes"`
}

// issueTeam is an issue's team as issueSelection selects it.
type issueTeam struct {
	ID                  string `json:"id"`
	IssueEstimationType string `json:"issueEstimationType"`
}

func (t *issueTeam) id() string {
	if t == nil {
		return ""
	}
	return t.ID
}

func (t *issueTeam) scale() EstimateScale {
	if t == nil {
		return ""
	}
	return EstimateScale(t.IssueEstimationType)
}

func (w wireIssue) typed() Issue {
	return Issue{
		ID: w.ID, Identifier: w.Identifier, Number: int(w.Number), Title: w.Title, Description: w.Description,
		URL: w.URL, BranchName: w.BranchName, Priority: Priority(w.Priority), Estimate: w.Estimate,
		SortOrder: w.SortOrder, PrioritySortOrder: w.PrioritySortOrder, SubIssueSortOrder: w.SubIssueSortOrder,
		TeamID: w.Team.id(), TeamEstimateScale: w.Team.scale(), StateID: w.State.id(), AssigneeID: w.Assignee.id(), CreatorID: w.Creator.id(),
		CycleID: w.Cycle.id(), ProjectID: w.Project.id(), ParentID: w.Parent.id(), LabelIDs: w.LabelIDs, Labels: w.Labels.Nodes,
		DueDate: w.DueDate, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, StartedAt: w.StartedAt,
		CompletedAt: w.CompletedAt, CanceledAt: w.CanceledAt, ArchivedAt: w.ArchivedAt,
		Trashed: w.Trashed != nil && *w.Trashed,
	}
}

const positionFields = `fragment PositionFields on Issue { id sortOrder }`

type wirePosition struct {
	ID        string  `json:"id"`
	SortOrder float64 `json:"sortOrder"`
}

func (w wirePosition) typed() IssuePosition { return IssuePosition(w) }

const issueRefFields = `fragment IssueRefFields on Issue { id identifier title state { id } }`

type wireIssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      *ref   `json:"state"`
}

func (w wireIssueRef) typed() IssueRef {
	return IssueRef{ID: w.ID, Identifier: w.Identifier, Title: w.Title, StateID: w.State.id()}
}

func (w *wireIssueRef) typedPtr() *IssueRef {
	if w == nil {
		return nil
	}
	r := w.typed()
	return &r
}

const commentFields = `fragment CommentFields on Comment {
  id issue { id } parent { id } user { id } body url createdAt updatedAt editedAt resolvedAt
}`

type wireComment struct {
	ID         string     `json:"id"`
	Issue      *ref       `json:"issue"`
	Parent     *ref       `json:"parent"`
	User       *ref       `json:"user"`
	Body       string     `json:"body"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	EditedAt   *time.Time `json:"editedAt"`
	ResolvedAt *time.Time `json:"resolvedAt"`
}

func (w wireComment) typed() Comment {
	return Comment{
		ID: w.ID, IssueID: w.Issue.id(), ParentID: w.Parent.id(), UserID: w.User.id(), Body: w.Body, URL: w.URL,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, EditedAt: w.EditedAt, ResolvedAt: w.ResolvedAt,
	}
}

const attachmentFields = `fragment AttachmentFields on Attachment {
  id issue { id } title subtitle url sourceType creator { id } createdAt updatedAt
}`

type wireAttachment struct {
	ID         string    `json:"id"`
	Issue      *ref      `json:"issue"`
	Title      string    `json:"title"`
	Subtitle   string    `json:"subtitle"`
	URL        string    `json:"url"`
	SourceType string    `json:"sourceType"`
	Creator    *ref      `json:"creator"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (w wireAttachment) typed() Attachment {
	return Attachment{
		ID: w.ID, IssueID: w.Issue.id(), Title: w.Title, Subtitle: w.Subtitle, URL: w.URL,
		SourceType: w.SourceType, CreatorID: w.Creator.id(), CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

const relationFields = `fragment RelationFields on IssueRelation {
  id type issue { ...IssueRefFields } relatedIssue { ...IssueRefFields }
}`

type wireRelation struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Issue        wireIssueRef `json:"issue"`
	RelatedIssue wireIssueRef `json:"relatedIssue"`
}

func (w wireRelation) typed() Relation {
	return Relation{ID: w.ID, Type: RelationType(w.Type), Issue: w.Issue.typed(), Related: w.RelatedIssue.typed()}
}

// historyFields selects the changes HistoryEntry types, by ID.
const historyFields = `fragment HistoryFields on IssueHistory {
  id createdAt updatedAt actorId
  fromStateId toStateId fromAssigneeId toAssigneeId fromCycleId toCycleId
  fromProjectId toProjectId fromParentId toParentId fromTeamId toTeamId
  fromEstimate toEstimate fromPriority toPriority fromTitle toTitle
  addedLabelIds removedLabelIds archived
}`

type wireHistory struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	ActorID         string    `json:"actorId"`
	FromStateID     *string   `json:"fromStateId"`
	ToStateID       *string   `json:"toStateId"`
	FromAssigneeID  *string   `json:"fromAssigneeId"`
	ToAssigneeID    *string   `json:"toAssigneeId"`
	FromCycleID     *string   `json:"fromCycleId"`
	ToCycleID       *string   `json:"toCycleId"`
	FromProjectID   *string   `json:"fromProjectId"`
	ToProjectID     *string   `json:"toProjectId"`
	FromParentID    *string   `json:"fromParentId"`
	ToParentID      *string   `json:"toParentId"`
	FromTeamID      *string   `json:"fromTeamId"`
	ToTeamID        *string   `json:"toTeamId"`
	FromEstimate    *float64  `json:"fromEstimate"`
	ToEstimate      *float64  `json:"toEstimate"`
	FromPriority    *float64  `json:"fromPriority"`
	ToPriority      *float64  `json:"toPriority"`
	FromTitle       *string   `json:"fromTitle"`
	ToTitle         *string   `json:"toTitle"`
	AddedLabelIDs   []string  `json:"addedLabelIds"`
	RemovedLabelIDs []string  `json:"removedLabelIds"`
	Archived        *bool     `json:"archived"`
}

func (w wireHistory) typed() HistoryEntry {
	return HistoryEntry{
		ID: w.ID, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, ActorID: w.ActorID,
		State: change(w.FromStateID, w.ToStateID), Assignee: change(w.FromAssigneeID, w.ToAssigneeID),
		Cycle: change(w.FromCycleID, w.ToCycleID), Project: change(w.FromProjectID, w.ToProjectID),
		Parent: change(w.FromParentID, w.ToParentID), Team: change(w.FromTeamID, w.ToTeamID),
		Estimate: numberChange(w.FromEstimate, w.ToEstimate), Priority: numberChange(w.FromPriority, w.ToPriority),
		Title: change(w.FromTitle, w.ToTitle), AddedLabelIDs: w.AddedLabelIDs, RemovedLabelIDs: w.RemovedLabelIDs,
		Archived: w.Archived,
	}
}

// change is the change an entry's pair records, or nil when it records
// none.
func change(from, to *string) *Change {
	if from == nil && to == nil {
		return nil
	}
	c := &Change{}
	if from != nil {
		c.From = *from
	}
	if to != nil {
		c.To = *to
	}
	return c
}

func numberChange(from, to *float64) *NumberChange {
	if from == nil && to == nil {
		return nil
	}
	return &NumberChange{From: from, To: to}
}

const stateSpanFields = `fragment StateSpanFields on IssueStateSpan { id stateId startedAt endedAt }`

type wireStateSpan struct {
	ID        string     `json:"id"`
	StateID   string     `json:"stateId"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
}

func (w wireStateSpan) typed() StateSpan {
	return StateSpan(w)
}
