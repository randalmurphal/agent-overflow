package linear

import "time"

// issueSelection is what every read of an issue selects. It leaves out
// the description, which is most of an issue's bytes (two thirds of a
// list page of ordinary issues, measured on the fake) and which only the
// single-issue reads carry (describedFields). Linear charged a page of
// 42 issues with these fields 8 points at first 100 and at 250
// (measured).
// Related entities are selected by ID, with two exceptions nested in the
// issue because they cost less than a second request. The team's estimate
// scale is a scalar on an object already selected, so a list of rows
// tells whether their estimates count without reading the team. The
// labels' names cost a page a few points whatever the nested page size
// or the labels it holds (measured), so the page is maxIssuePage:
// only an issue with more labels than that, or with one the list leaves
// out, needs LabelsByID.
// labelIds stays the complete set.
const issueSelection = `
  id identifier number title url branchName
  priority estimate sortOrder prioritySortOrder subIssueSortOrder
  team { id issueEstimationType } state { id } assignee { id } creator { id } cycle { id } project { id } parent { id }
  labelIds labels(first: 250) { nodes { id name } }
  dueDate createdAt updatedAt startedAt completedAt canceledAt archivedAt trashed
`

const issueFields = `fragment IssueFields on Issue {` + issueSelection + `}`

// issueSearchFields is issueSelection on a search result, a type of its
// own with the same fields, and the state and assignee whole: a search
// row is shown with their names, whatever the app has read of the team.
// A page of issues that selects objects costs the same however many it
// selects (measured, docs/references/tracker-api-measurements.md), so
// the names cost bytes and no points. Its documents spread StateFields
// and UserFields too.
const issueSearchFields = `fragment IssueSearchFields on IssueSearchResult {` + issueSelection + `  state { ...StateFields } assignee { ...UserFields }
}`

// wireSearchIssue is an issue as issueSearchFields selects it; its state
// and assignee shadow issueSelection's { id } ones.
type wireSearchIssue struct {
	wireIssue
	State    *wireState `json:"state"`
	Assignee *User      `json:"assignee"`
}

func (w wireSearchIssue) typed() SearchHit {
	hit := SearchHit{Issue: w.wireIssue.typed(), Assignee: w.Assignee}
	if w.State != nil {
		state := w.State.typed()
		hit.State, hit.StateID = &state, state.ID
	}
	if w.Assignee != nil {
		hit.AssigneeID = w.Assignee.ID
	}
	return hit
}

// describedFields is issueSelection with the description, for the reads
// of one issue whose callers show or compare it.
const describedFields = `fragment DescribedFields on Issue { ...IssueFields description }`

// wireDescribed is an issue as describedFields selects it.
type wireDescribed struct {
	wireIssue
	Description string `json:"description"`
}

func (w wireDescribed) typed() DescribedIssue {
	return DescribedIssue{Issue: w.wireIssue.typed(), Description: w.Description}
}

type wireIssue struct {
	ID                string     `json:"id"`
	Identifier        string     `json:"identifier"`
	Number            float64    `json:"number"`
	Title             string     `json:"title"`
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
		ID: w.ID, Identifier: w.Identifier, Number: int(w.Number), Title: w.Title,
		URL: w.URL, BranchName: w.BranchName, Priority: Priority(w.Priority), Estimate: w.Estimate,
		SortOrder: w.SortOrder, PrioritySortOrder: w.PrioritySortOrder, SubIssueSortOrder: w.SubIssueSortOrder,
		TeamID: w.Team.id(), TeamEstimateScale: w.Team.scale(), StateID: w.State.id(), AssigneeID: w.Assignee.id(), CreatorID: w.Creator.id(),
		CycleID: w.Cycle.id(), ProjectID: w.Project.id(), ParentID: w.Parent.id(), LabelIDs: w.LabelIDs, Labels: w.Labels.Nodes,
		DueDate: w.DueDate, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, StartedAt: w.StartedAt,
		CompletedAt: w.CompletedAt, CanceledAt: w.CanceledAt, ArchivedAt: w.ArchivedAt,
		Trashed: w.Trashed != nil && *w.Trashed,
	}
}

const positionFields = `fragment PositionFields on Issue { id sortOrder team { id } }`

type wirePosition struct {
	ID        string  `json:"id"`
	SortOrder float64 `json:"sortOrder"`
	Team      *ref    `json:"team"`
}

func (w wirePosition) typed() IssuePosition {
	return IssuePosition{ID: w.ID, SortOrder: w.SortOrder, TeamID: w.Team.id()}
}

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

// namedCommentFields is commentFields with its author whole, for the
// reads that show comments. Its documents spread UserFields too.
const namedCommentFields = `fragment NamedCommentFields on Comment {
  ...CommentFields
  user { ...UserFields }
}`

// wireNamedComment is a comment as namedCommentFields selects it; its user
// shadows commentFields' { id } one.
type wireNamedComment struct {
	wireComment
	User *User `json:"user"`
}

func (w wireNamedComment) typed() NamedComment {
	c := NamedComment{Comment: w.wireComment.typed(), User: w.User}
	if w.User != nil {
		c.UserID = w.User.ID
	}
	return c
}

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

// namedHistoryFields is historyFields with what a ticket shows of each
// entity an entry names, for the reads of a ticket's history
// (NamedEntry). Each nested object costs about 125 points per 100 entries
// of the page (1,500 for these twelve at history(first: 100), measured),
// so it selects only what an event shows: a user's name and display
// name, a cycle's number and name, a project's name, a team's key and
// name, a parent's identifier and the labels' names. A workflow state is
// named by the team's states the ticket read holds. A document that
// spreads it carries historyFields too.
const namedHistoryFields = `fragment NamedHistoryFields on IssueHistory {
  ...HistoryFields
  actor { ...UserFields }
  fromAssignee { id name displayName } toAssignee { id name displayName }
  fromCycle { id number name } toCycle { id number name }
  fromProject { id name } toProject { id name }
  fromTeam { id key name } toTeam { id key name }
  fromParent { id identifier } toParent { id identifier }
  addedLabels { id name } removedLabels { id name }
}`

type wireNamedHistory struct {
	wireHistory
	Actor         *User         `json:"actor"`
	FromAssignee  *UserRef      `json:"fromAssignee"`
	ToAssignee    *UserRef      `json:"toAssignee"`
	FromCycle     *wireCycleRef `json:"fromCycle"`
	ToCycle       *wireCycleRef `json:"toCycle"`
	FromProject   *ProjectRef   `json:"fromProject"`
	ToProject     *ProjectRef   `json:"toProject"`
	FromTeam      *TeamRef      `json:"fromTeam"`
	ToTeam        *TeamRef      `json:"toTeam"`
	FromParent    *IssueKey     `json:"fromParent"`
	ToParent      *IssueKey     `json:"toParent"`
	AddedLabels   []LabelRef    `json:"addedLabels"`
	RemovedLabels []LabelRef    `json:"removedLabels"`
}

// wireCycleRef is a cycle as namedHistoryFields selects it.
type wireCycleRef struct {
	ID     string  `json:"id"`
	Number float64 `json:"number"`
	Name   string  `json:"name"`
}

func (w *wireCycleRef) typed() *CycleRef {
	if w == nil {
		return nil
	}
	return &CycleRef{ID: w.ID, Number: int(w.Number), Name: w.Name}
}

func (w wireNamedHistory) typed() NamedEntry {
	return NamedEntry{
		HistoryEntry: w.wireHistory.typed(), Actor: w.Actor,
		FromAssignee: w.FromAssignee, ToAssignee: w.ToAssignee,
		FromCycle: w.FromCycle.typed(), ToCycle: w.ToCycle.typed(),
		FromProject: w.FromProject, ToProject: w.ToProject,
		FromTeam: w.FromTeam, ToTeam: w.ToTeam,
		FromParent: w.FromParent, ToParent: w.ToParent,
		AddedLabels: w.AddedLabels, RemovedLabels: w.RemovedLabels,
	}
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
