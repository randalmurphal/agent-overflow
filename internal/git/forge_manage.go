package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// Managing a PR: its title and description, reviewers, assignees and
// labels, closing, reopening and converting it to a draft, re-requesting
// a review and clearing a review that requested changes (GitHub dismisses
// it, GitLab bypasses it). The read side rides the detail read
// (PRDetail.Manage); the writes are ForgeManager, each confirmed by the
// PR pump's next poll as the merge actions are. See
// docs/architecture/forge-transport.md, "Managing a PR".

// Reviewer states, the vocabulary of ReviewVerdict.State and the
// Conversation feed.
const (
	ReviewerApproved         = "APPROVED"
	ReviewerChangesRequested = "CHANGES_REQUESTED"
	ReviewerCommented        = "COMMENTED"
	ReviewerPending          = "PENDING"
)

// PRManage is what the review pane's Details section shows and which
// management controls it offers, read with the detail. Unavailable is
// the sentence saying the read failed: the lists are then empty and
// every Can flag false, and the rest of the detail stands.
type PRManage struct {
	Reviewers   []PRReviewer `json:"reviewers"`
	Assignees   []PRPerson   `json:"assignees"`
	Labels      []PRLabel    `json:"labels"`
	Can         PRManageCan  `json:"can"`
	Unavailable string       `json:"unavailable,omitempty"`
}

// emptyManage is a PRManage with nothing shown and nothing offered.
func emptyManage() PRManage {
	return PRManage{Reviewers: []PRReviewer{}, Assignees: []PRPerson{}, Labels: []PRLabel{}}
}

// PRPerson is a user, or on GitHub a team (Team, Login its
// "org/slug"). Both forges address people by Login; ID is GitLab's
// numeric user id, which its re-review mutation alone takes.
type PRPerson struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
	ID    string `json:"id,omitempty"`
	Team  bool   `json:"team,omitempty"`
}

// PRReviewer is one reviewer and where their review stands: State is
// ReviewerApproved, ReviewerChangesRequested, ReviewerCommented or
// ReviewerPending (asked and not reviewed since). Stale is a GitHub
// review on a commit older than the head.
type PRReviewer struct {
	PRPerson
	State string `json:"state"`
	Stale bool   `json:"stale,omitempty"`
}

// PRLabel is one label. ID is its global id (a GitHub node id, a GitLab
// label gid), which label writes address it by on both forges. Color is
// "#rrggbb", empty when the forge gave none.
type PRLabel struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// PRManageCan is what the forge says the viewer may do, one flag per
// control; a false flag hides the control. Each comes from the forge's
// own answer (GitHub viewerCan* and viewerPermission, GitLab
// userPermissions and user.can_merge) and applies to the PR's current
// state (Close only while open, Reopen only while closed, ConvertToDraft
// only on an open PR that is not a draft). Rerequest also needs the
// server's re-review mutation, Bypass GitLab's override (both read from
// its schema). ReviewerTeams is whether reviewer search can offer teams:
// GitHub organizations only.
type PRManageCan struct {
	EditTitle       bool `json:"editTitle"`
	EditDescription bool `json:"editDescription"`
	Reviewers       bool `json:"reviewers"`
	Assignees       bool `json:"assignees"`
	Labels          bool `json:"labels"`
	Close           bool `json:"close"`
	Reopen          bool `json:"reopen"`
	ConvertToDraft  bool `json:"convertToDraft"`
	Rerequest       bool `json:"rerequest"`
	Dismiss         bool `json:"dismiss"`
	Bypass          bool `json:"bypass"`
	ReviewerTeams   bool `json:"reviewerTeams"`
}

// ChangeRequest is one review that requested changes and blocks the
// merge, on a BlockerChangesRequested row: ReviewID is the GitHub review
// node id DismissPRReview takes.
type ChangeRequest struct {
	ReviewID    string `json:"reviewId"`
	AuthorLogin string `json:"authorLogin"`
	AuthorName  string `json:"authorName,omitempty"`
}

// PREdit is a title or description edit; nil leaves a field as it is.
// A description edit names ExpectedDescription, the description the
// editor opened with: the forge adapter re-reads the description and
// refuses with ErrDescriptionChanged when it differs (line endings
// aside), since neither forge's API takes a precondition on it.
type PREdit struct {
	Title               *string `json:"title,omitempty"`
	Description         *string `json:"description,omitempty"`
	ExpectedDescription *string `json:"expectedDescription,omitempty"`
}

// ErrDescriptionChanged refuses a description edit whose
// ExpectedDescription is no longer the PR's.
var ErrDescriptionChanged = &ForgeActionError{Kind: ForgeErrorRefused, Message: "The description changed since you opened it. Copy your text, reload the description and edit again."}

// Validate checks the edit's shape.
func (e PREdit) Validate() error {
	switch {
	case e.Title == nil && e.Description == nil:
		return errors.New("the edit changes nothing")
	case e.Title != nil && strings.TrimSpace(*e.Title) == "":
		return errors.New("the title cannot be empty")
	case e.Title != nil && strings.ContainsAny(*e.Title, "\r\n"):
		return errors.New("the title is one line")
	case e.Description != nil && e.ExpectedDescription == nil:
		return errors.New("a description edit names the description it replaces")
	case e.Description == nil && e.ExpectedDescription != nil:
		return errors.New("the expected description belongs to a description edit")
	}
	return nil
}

// sameDescription compares two descriptions as the forges store them:
// GitHub answers a body edited in the browser with CRLF line endings.
func sameDescription(a, b string) bool {
	norm := func(s string) string { return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n") }
	return norm(a) == norm(b)
}

// PRPeopleChange removes, then adds, reviewers or assignees, one write
// per side. A GitHub team (reviewers only) is a PRPerson with Team.
type PRPeopleChange struct {
	Add    []PRPerson `json:"add"`
	Remove []PRPerson `json:"remove"`
}

// prPeopleMax bounds one change, as the forges bound reviewers and
// assignees.
const prPeopleMax = 100

// Validate checks the change for a forge: something to do, at most
// prPeopleMax people, each a login, no team outside GitHub reviewers,
// and no one both added and removed.
func (c PRPeopleChange) Validate(forge string, reviewers bool) error {
	if len(c.Add)+len(c.Remove) == 0 {
		return errors.New("the change adds and removes no one")
	}
	if len(c.Add)+len(c.Remove) > prPeopleMax {
		return fmt.Errorf("a change names at most %d people", prPeopleMax)
	}
	seen := map[string]bool{}
	for _, person := range append(append([]PRPerson{}, c.Add...), c.Remove...) {
		if err := validPerson(forge, person, reviewers, false); err != nil {
			return err
		}
		key := strings.ToLower(person.Login)
		if seen[key] {
			return fmt.Errorf("%s is named twice", person.Login)
		}
		seen[key] = true
	}
	return nil
}

// validPerson checks one person; needID asks for GitLab's user id, which
// its re-review takes.
func validPerson(forge string, person PRPerson, reviewers, needID bool) error {
	login := strings.TrimSpace(person.Login)
	if login == "" || login != person.Login || strings.ContainsAny(login, " \t\r\n,") {
		return fmt.Errorf("%q is not a login", person.Login)
	}
	if person.Team && (forge != "github" || !reviewers || strings.Count(login, "/") != 1) {
		return fmt.Errorf("%s is not a GitHub team reviewer", login)
	}
	if forge == "gitlab" && needID {
		if _, err := gitlabUserID(person.ID); err != nil {
			return fmt.Errorf("%s: %w", login, err)
		}
	}
	return nil
}

// gitlabUserID parses a GitLab numeric user id.
func gitlabUserID(id string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("user id %q is not a GitLab user id", id)
	}
	return n, nil
}

// PRLabelChange removes, then adds, labels, one write per side, each
// label by its ID.
type PRLabelChange struct {
	Add    []PRLabel `json:"add"`
	Remove []PRLabel `json:"remove"`
}

// Validate checks the change's shape.
func (c PRLabelChange) Validate() error {
	if len(c.Add)+len(c.Remove) == 0 {
		return errors.New("the change adds and removes no label")
	}
	if len(c.Add)+len(c.Remove) > prPeopleMax {
		return fmt.Errorf("a change names at most %d labels", prPeopleMax)
	}
	seen := map[string]bool{}
	for _, label := range append(append([]PRLabel{}, c.Add...), c.Remove...) {
		key := label.ID
		switch {
		case strings.TrimSpace(key) == "":
			return fmt.Errorf("label %q carries no id", label.Name)
		case seen[key]:
			return fmt.Errorf("label %s is named twice", firstNonEmpty(label.Name, key))
		}
		seen[key] = true
	}
	return nil
}

// PRStateChange values: what SetPRState does.
const (
	PRStateClose   = "close"
	PRStateReopen  = "reopen"
	PRStateToDraft = "draft"
)

// ValidatePRStateChange checks a SetPRState argument.
func ValidatePRStateChange(state string) error {
	switch state {
	case PRStateClose, PRStateReopen, PRStateToDraft:
		return nil
	}
	return fmt.Errorf("unsupported PR state change %q", state)
}

// Candidate search kinds.
const (
	CandidateReviewers = "reviewers"
	CandidateAssignees = "assignees"
	CandidateLabels    = "labels"
)

// candidatePageSize is the results one search answers; past it More is
// set and the picker asks the user to type to narrow.
const candidatePageSize = 20

// candidateQueryMax bounds a search string.
const candidateQueryMax = 100

// PRCandidates is one picker search: the first candidatePageSize people
// (users, and GitHub teams for reviewers when the login can read them)
// or labels matching the query, and More when the forge has more.
type PRCandidates struct {
	People []PRPerson `json:"people"`
	Labels []PRLabel  `json:"labels"`
	More   bool       `json:"more"`
}

// ValidateCandidateSearch checks a search's kind and query.
func ValidateCandidateSearch(kind, query string) error {
	switch kind {
	case CandidateReviewers, CandidateAssignees, CandidateLabels:
	default:
		return fmt.Errorf("unsupported search %q", kind)
	}
	if len(query) > candidateQueryMax || strings.ContainsAny(query, "\r\n") {
		return fmt.Errorf("a search is one line of at most %d bytes", candidateQueryMax)
	}
	return nil
}

// MergeMessage is a merge commit's message: its title (the first line)
// and body.
type MergeMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// String is the whole message, as GitLab takes it.
func (m MergeMessage) String() string {
	if strings.TrimSpace(m.Body) == "" {
		return m.Title
	}
	return m.Title + "\n\n" + m.Body
}

// splitMergeMessage splits a whole message into its first line and the
// rest.
func splitMergeMessage(message string) MergeMessage {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	title, body, _ := strings.Cut(message, "\n")
	return MergeMessage{Title: strings.TrimSpace(title), Body: strings.Trim(body, "\n")}
}

// methodMakesCommit is whether a merge method writes a commit whose
// message can be edited: a rebase (GitLab's fast-forward too) does not.
func methodMakesCommit(method string) bool {
	return method == MergeMethodMerge || method == MergeMethodSquash
}

// ValidateMergeMessageMethod checks a default-message read's method.
func ValidateMergeMessageMethod(method string) error {
	if !methodMakesCommit(method) {
		return fmt.Errorf("the %s method writes no commit message", method)
	}
	return nil
}

// UnconfirmedWriteError is a management write the forge may have
// applied: it failed in a way forgeapi.WriteNotApplied cannot rule out (a
// reset or timeout after sending, a 5xx, a body cut short, an error
// beside data). A caller does not send it again; it refreshes and lets
// the forge's answer show what happened.
type UnconfirmedWriteError struct {
	Err error
}

func (e *UnconfirmedWriteError) Error() string {
	return fmt.Sprintf("the forge did not confirm the change: %v", e.Err)
}

func (e *UnconfirmedWriteError) Unwrap() error { return e.Err }

// PartialWriteError is a change whose earlier writes took effect (Done,
// "removed bob from the reviewers") and whose next write (Next, "adding
// carol") failed with Err, which either refused it or left it
// unconfirmed (Unconfirmed). The caller refreshes as for an unconfirmed
// write.
type PartialWriteError struct {
	Done, Next string
	Err        error
}

func (e *PartialWriteError) Error() string {
	return fmt.Sprintf("%s; %s failed: %v", e.Done, e.Next, e.Err)
}

func (e *PartialWriteError) Unwrap() error { return e.Err }

// Unconfirmed is whether the failed write may have applied: anything but
// a refusal (an explained ForgeActionError, or what
// forgeapi.WriteNotApplied proves).
func (e *PartialWriteError) Unconfirmed() bool {
	if _, ok := errors.AsType[*UnconfirmedWriteError](e.Err); ok {
		return true
	}
	if _, ok := errors.AsType[*ForgeActionError](e.Err); ok {
		return false
	}
	return !forgeapi.WriteNotApplied(e.Err)
}

// changeStep is one write of a change: done words it applied ("removed
// bob from the reviewers"), next words it to come ("adding carol").
type changeStep struct {
	done, next string
	run        func() error
}

// runSteps runs a change's writes in order and stops at the first
// failure: the first write's failure classified as writeOutcome does,
// a later one's a PartialWriteError after what the earlier ones did.
func runSteps(steps []changeStep) error {
	var done []string
	for _, step := range steps {
		if err := step.run(); err != nil {
			if len(done) == 0 {
				if _, explained := errors.AsType[*ForgeActionError](err); explained {
					return err
				}
				return writeOutcome(err)
			}
			return &PartialWriteError{Done: strings.Join(done, "; "), Next: step.next, Err: err}
		}
		done = append(done, step.done)
	}
	return nil
}

// stepWords words one side of a people or label change: "removed bob
// and carol from the reviewers" and "removing bob and carol".
func stepWords(verb string, names []string, list string) (done, next string) {
	joined := joinClauses(names)
	switch verb {
	case "remove":
		return fmt.Sprintf("removed %s from the %s", joined, list), "removing " + joined
	default:
		return fmt.Sprintf("added %s to the %s", joined, list), "adding " + joined
	}
}

// writeOutcome is the error of one management write as its caller
// classifies it: nil, as it is when the forge did not apply it, else
// unconfirmed.
func writeOutcome(err error) error {
	if _, classified := errors.AsType[*UnconfirmedWriteError](err); classified || err == nil || forgeapi.WriteNotApplied(err) {
		return err
	}
	return &UnconfirmedWriteError{Err: err}
}

// ForgeManager is the management half of Forge. Every write takes an
// interactive context and is checked inside the adapter against the PR
// it names: an id the caller passes (a review to dismiss, a label to
// remove) must belong to that PR.
type ForgeManager interface {
	// EditPR changes the title and/or the description (see PREdit).
	EditPR(ctx context.Context, ref PRReference, edit PREdit) error
	// SetPRPeople adds and removes reviewers (reviewers) or assignees.
	SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error
	// SetPRLabels adds and removes labels.
	SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error
	// SetPRState closes, reopens or converts the PR to a draft.
	SetPRState(ctx context.Context, ref PRReference, state string) error
	// RerequestReview asks reviewer for a new review.
	RerequestReview(ctx context.Context, ref PRReference, reviewer PRPerson) error
	// DismissReview dismisses a review that requested changes (GitHub;
	// GitLab answers ErrUnsupportedForge).
	DismissReview(ctx context.Context, ref PRReference, reviewID, message string) error
	// BypassRequestedChanges overrides the requested changes blocking the
	// merge (GitLab; GitHub answers ErrUnsupportedForge).
	BypassRequestedChanges(ctx context.Context, ref PRReference) error
	// SearchCandidates is one picker search (see PRCandidates).
	SearchCandidates(ctx context.Context, ref PRReference, kind, query string) (PRCandidates, error)
	// DefaultMergeMessage is the forge's default commit message of a
	// merge with method (merge or squash).
	DefaultMergeMessage(ctx context.Context, ref PRReference, method string) (MergeMessage, error)
}

func (nullForge) EditPR(context.Context, PRReference, PREdit) error { return ErrUnsupportedForge }
func (nullForge) SetPRPeople(context.Context, PRReference, bool, PRPeopleChange) error {
	return ErrUnsupportedForge
}
func (nullForge) SetPRLabels(context.Context, PRReference, PRLabelChange) error {
	return ErrUnsupportedForge
}
func (nullForge) SetPRState(context.Context, PRReference, string) error { return ErrUnsupportedForge }
func (nullForge) RerequestReview(context.Context, PRReference, PRPerson) error {
	return ErrUnsupportedForge
}
func (nullForge) DismissReview(context.Context, PRReference, string, string) error {
	return ErrUnsupportedForge
}
func (nullForge) BypassRequestedChanges(context.Context, PRReference) error {
	return ErrUnsupportedForge
}
func (nullForge) SearchCandidates(context.Context, PRReference, string, string) (PRCandidates, error) {
	return PRCandidates{}, ErrUnsupportedForge
}
func (nullForge) DefaultMergeMessage(context.Context, PRReference, string) (MergeMessage, error) {
	return MergeMessage{}, ErrUnsupportedForge
}

// EditPR validates and dispatches a title or description edit.
func (c *Core) EditPR(ctx context.Context, ref PRReference, edit PREdit) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := edit.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).EditPR(forgeWrite(ctx), ref, edit)
}

// SetPRPeople validates and dispatches a reviewer or assignee change.
func (c *Core) SetPRPeople(ctx context.Context, ref PRReference, reviewers bool, change PRPeopleChange) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := change.Validate(ref.Forge, reviewers); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).SetPRPeople(forgeWrite(ctx), ref, reviewers, change)
}

// SetPRLabels validates and dispatches a label change.
func (c *Core) SetPRLabels(ctx context.Context, ref PRReference, change PRLabelChange) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := change.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).SetPRLabels(forgeWrite(ctx), ref, change)
}

// SetPRState validates and dispatches a close, reopen or convert to
// draft.
func (c *Core) SetPRState(ctx context.Context, ref PRReference, state string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ValidatePRStateChange(state); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).SetPRState(forgeWrite(ctx), ref, state)
}

// RerequestPRReview validates and dispatches a re-request.
func (c *Core) RerequestPRReview(ctx context.Context, ref PRReference, reviewer PRPerson) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := validPerson(ref.Forge, reviewer, true, true); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).RerequestReview(forgeWrite(ctx), ref, reviewer)
}

// DismissPRReview validates and dispatches a dismissal: GitHub requires
// a message.
func (c *Core) DismissPRReview(ctx context.Context, ref PRReference, reviewID, message string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(reviewID) == "" {
		return errors.New("a dismissal names the review")
	}
	if strings.TrimSpace(message) == "" {
		return errors.New("a dismissal needs a reason")
	}
	return c.ForgeByID(ref.Forge).DismissReview(forgeWrite(ctx), ref, reviewID, strings.TrimSpace(message))
}

// BypassPRRequestedChanges dispatches a bypass.
func (c *Core) BypassPRRequestedChanges(ctx context.Context, ref PRReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).BypassRequestedChanges(forgeWrite(ctx), ref)
}

// SearchPRCandidates validates and dispatches one picker search. It is
// a read on the caller's context.
func (c *Core) SearchPRCandidates(ctx context.Context, ref PRReference, kind, query string) (PRCandidates, error) {
	if err := ref.Validate(); err != nil {
		return PRCandidates{}, err
	}
	if err := ValidateCandidateSearch(kind, query); err != nil {
		return PRCandidates{}, err
	}
	return c.ForgeByID(ref.Forge).SearchCandidates(ctx, ref, kind, strings.TrimSpace(query))
}

// PRMergeMessage reads the forge's default commit message for method.
func (c *Core) PRMergeMessage(ctx context.Context, ref PRReference, method string) (MergeMessage, error) {
	if err := ref.Validate(); err != nil {
		return MergeMessage{}, err
	}
	if err := ValidateMergeMessageMethod(method); err != nil {
		return MergeMessage{}, err
	}
	return c.ForgeByID(ref.Forge).DefaultMergeMessage(ctx, ref, method)
}

// normalizeColor spells a forge's label color as "#rrggbb": GitHub
// answers it without the hash.
func normalizeColor(color string) string {
	color = strings.TrimSpace(color)
	if color == "" {
		return ""
	}
	if !strings.HasPrefix(color, "#") {
		color = "#" + color
	}
	return strings.ToLower(color)
}

// manageUnavailableSentence words a failed Details read
// (PRManage.Unavailable) in the error vocabulary.
func manageUnavailableSentence(ref PRReference, err error) string {
	const what = "Reviewers, assignees, labels and the PR's edit controls are"
	labels := forgeLabels(ref.Forge)
	if forbidden, ok := errors.AsType[*forgeapi.ForbiddenError](err); ok && forbidden.Scope != "" {
		return fmt.Sprintf("%s not shown: your %s token lacks the `%s` scope. Create a token with that scope and log %s in with it.", what, labels.host(ref.Host), forbidden.Scope, labels.binary)
	}
	explained, _ := ExplainForgeError(ref, ActionRead, err).(*ForgeActionError)
	return what + " not shown for now. " + explained.Message
}
