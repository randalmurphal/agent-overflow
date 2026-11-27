package trackerapp

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A project's tracker setup: the per-project choices of the Issues
// settings beyond its boards, in one file in the project's app directory
// (project.ConfigDir, beside its workflow profile), which the service
// reads and replaces whole. The file is the only durable copy. Its
// sections are a JSON object's keys, each carrying its own version:
//
//   - status_groups: by tracker site, the group a status counts in, a
//     tracker group or left out of counts, which the board's sections and
//     totals take (StatusGroups);
//   - ticket_pins: by tracker site, the fields and the sections a ticket
//     shows first;
//   - start_work: by tracker site and tracker project, what Start work
//     does to a ticket and where its thread runs (StartWorkDefaults);
//   - agents: what a thread of the project may do to tickets through the
//     tracker tools (AgentRules);
//   - time: the Jira tickets meeting and time-off hours go on, and
//     whether an agent's time draft may put hours on an epic
//     (TimeSettings);
//   - estimates: whether the project counts its Jira boards' work by
//     their estimates or by their tickets, and whether its tickets offer
//     planning poker (Estimates);
//   - credit: whether the project counts credit, the rules it counts it
//     by as they changed over time, and by Jira site which tickets are
//     support (Credit);
//   - dashboards: by tracker site and board, the dashboards the project
//     keeps (BoardDashboards). It is not part of Setup: a replace keeps
//     it as held, and only its own edits write it (dashboard_setup.go), so
//     a screen that saves the setup it read never carries it.
//
// A section this build does not know, or one at a version newer than it
// reads, is kept byte for byte and written back unchanged by a replace,
// which may not change it (ErrSetupNewer); a replace's setup argument
// that names one is refused as unreadable (Setup.UnmarshalJSON). A site
// is held as its connection names it (checkSite). The revision is the SHA-256 of
// the file's bytes, empty while there is no file; a replace names the
// revision it was built from and is refused, with what is stored now, when
// the stored one differs. A file that cannot be read or does not validate
// is refused as what it is, never read as empty and never overwritten.
//
// The service holds each used project's setup in memory, by slug, under
// setupMu: the file as the service last saw it, loaded at its first use.
// A save reads the file again and holds what it saw: what it read, or
// what it wrote. A save that cannot read the file, or whose write or
// removal failed, holds nothing, and neither does a first use that cannot
// read it, so the next use reads the file again. A file edited by hand
// while the app runs is read by the next save or the next start.

const (
	// setupFileName is the setup's file in the project's app directory.
	setupFileName = "tracker.json"
	// setupMaxBytes bounds the file, and so the memory a setup holds.
	setupMaxBytes = 256 << 10
	// The bounds of one site's entry.
	setupMaxStatuses = 1000
	setupMaxFields   = 200
	setupMaxSections = 100
	// setupMaxStartWork bounds the start_work entries, and
	// setupMaxBranchTemplate a branch template.
	setupMaxStartWork      = 200
	setupMaxBranchTemplate = 100
	// setupMaxID bounds a site, a status, a field or a section ID.
	setupMaxID = 255
	// The sections this build reads, and the version of each it writes.
	sectionStatusGroups = "status_groups"
	sectionTicketPins   = "ticket_pins"
	sectionStartWork    = "start_work"
	sectionAgents       = "agents"
	sectionTime         = "time"
	sectionEstimates    = "estimates"
	sectionCredit       = "credit"
	sectionDashboards   = "dashboards"
	setupSectionVersion = 1
)

var (
	// ErrSetupUnreadable is a setup file that cannot be read, or that does
	// not validate; SetupFileError says which and why.
	ErrSetupUnreadable = errors.New("trackerapp: the project's tracker setup file cannot be read")
	// ErrSetupNewer is a replace that changes a section a newer version of
	// the app saved.
	ErrSetupNewer = errors.New("trackerapp: a newer version of the app saved this part of the tracker setup")
	// ErrInvalidSetup is a setup a replace gives that does not validate;
	// SetupError says why.
	ErrInvalidSetup = errors.New("trackerapp: invalid tracker setup")
	// ErrSetupWriteUnsure is a replace whose write or removal of the file
	// failed, after which the file may hold the change or not: a failed
	// directory sync follows the rename or the unlink. SetupWriteError
	// says which file and why.
	ErrSetupWriteUnsure = errors.New("trackerapp: the project's tracker setup file may or may not hold this change")
)

// SetupFileError is a setup file that cannot be read or does not
// validate, with its path and why.
type SetupFileError struct {
	Path, Reason string
}

func (e *SetupFileError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrSetupUnreadable, e.Path, e.Reason)
}

// Is matches ErrSetupUnreadable.
func (e *SetupFileError) Is(target error) bool { return target == ErrSetupUnreadable }

// SetupWriteError is a replace whose write or removal of the file at Path
// failed, with why. The file holds the change or the setup before it.
type SetupWriteError struct {
	Path string
	Err  error
}

func (e *SetupWriteError) Error() string {
	return fmt.Sprintf("%s: %s: %v", ErrSetupWriteUnsure, e.Path, e.Err)
}

// Is matches ErrSetupWriteUnsure.
func (e *SetupWriteError) Is(target error) bool { return target == ErrSetupWriteUnsure }

func (e *SetupWriteError) Unwrap() error { return e.Err }

// SetupError is a setup that does not validate, with why; Full when
// what does not fit is the file's bound (setupMaxBytes).
type SetupError struct {
	Reason string
	Full   bool
}

func (e *SetupError) Error() string { return ErrInvalidSetup.Error() + ": " + e.Reason }

// Is matches ErrInvalidSetup.
func (e *SetupError) Is(target error) bool { return target == ErrInvalidSetup }

func invalidSetup(format string, args ...any) error {
	return &SetupError{Reason: fmt.Sprintf(format, args...)}
}

// Setup is the part of a project's tracker setup this build reads: the
// sections a replace gives and a read answers. A site with no entry in a
// section has nothing set there.
type Setup struct {
	StatusGroups []SiteStatusGroups  `json:"status_groups"`
	TicketPins   []SitePins          `json:"ticket_pins"`
	StartWork    []StartWorkDefaults `json:"start_work"`
	Agents       *AgentRules         `json:"agents,omitempty"`
	Time         *TimeSettings       `json:"time,omitempty"`
	Estimates    *Estimates          `json:"estimates,omitempty"`
	Credit       *Credit             `json:"credit,omitempty"`
}

// UnmarshalJSON reads a replace's setup whole or refuses it: a JSON object
// of the sections this build reads, each entry holding only its own
// fields, as the file's reader refuses a field of a section it reads
// (decodeStrict). Null, anything but an object, a section this build does
// not read (a newer app's) and a field it does not know are refused, so a
// setup is never saved as less than it says; {} is the empty setup.
func (s *Setup) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("trackerapp: a tracker setup is a JSON object of its sections")
	}
	type fields Setup
	var out fields
	if err := decodeStrict(data, &out); err != nil {
		return fmt.Errorf("trackerapp: read the tracker setup: %w", err)
	}
	*s = Setup(out)
	return nil
}

// SiteStatusGroups is the group each status of one tracker site counts in
// for the project, by status ID: backlog, todo, in_progress, done,
// canceled, or left_out (out of counts: the status's rows show in their
// columns and sprints, are not in the backlog, and add to no total). A
// status it does not name counts in the tracker's group.
type SiteStatusGroups struct {
	Tracker  string            `json:"tracker"`
	Site     string            `json:"site"`
	Statuses map[string]string `json:"statuses"`
}

// SitePins is what a ticket of one tracker site shows first for the
// project: its pinned fields (FieldView.ID), in order, and its pinned
// sections (a rich-text field's ID, description, links or attachments),
// in order. Absent means not set, and the ticket shows its default; an
// empty list means nothing pinned. A Linear site pins no fields: its field
// list is fixed.
type SitePins struct {
	Tracker  string    `json:"tracker"`
	Site     string    `json:"site"`
	Fields   *[]string `json:"fields,omitempty"`
	Sections *[]string `json:"sections,omitempty"`
}

// StartWorkDefaults is what Start work does for the project to a ticket of
// one tracker project (Project, its key's prefix: WEB of WEB-1, a Linear
// team's key) of one tracker site, each absent field meaning its built-in
// default: Status, the status the ticket moves to (absent: it does not
// move); Assign, whether the ticket is assigned to the connection's
// account (absent: it is); Branch, the template of the branch's name,
// whose {key} is the ticket's key and {title} its title made a branch
// name (absent: {key}-{title}); NameBranch, whether the Start work form
// offers that name or an empty branch box for the person to fill (absent:
// it offers the name); Repository, the app project the thread runs in
// (absent: this project). The tracker project is the one identity
// Jira and Linear share that a ticket carries, and a status belongs to a
// project's workflow or a team, so the entry is per tracker project.
type StartWorkDefaults struct {
	Tracker    string `json:"tracker"`
	Site       string `json:"site"`
	Project    string `json:"project"`
	Status     string `json:"status,omitempty"`
	Assign     *bool  `json:"assign,omitempty"`
	Branch     string `json:"branch,omitempty"`
	NameBranch *bool  `json:"name_branch,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// AgentRules is what a thread of the project may do to tickets through
// the tracker tools, each absent field meaning the built-in rule:
// ChangeTickets, whether a thread that is not read-only may create,
// change and comment on tickets (absent: it may); AskSeveral, whether a
// change to more than one ticket waits for the person's approval
// (absent: it does). Reading tickets is never refused. The rules are the
// calling thread's project's, whichever connection the ticket is on. A
// setup with no rules set has none (nil), which is no section.
type AgentRules struct {
	ChangeTickets *bool `json:"change_tickets,omitempty"`
	AskSeveral    *bool `json:"ask_several,omitempty"`
}

// Changes reports whether a thread may change tickets.
func (r AgentRules) Changes() bool { return r.ChangeTickets == nil || *r.ChangeTickets }

// Asks reports whether a change to several tickets waits for approval.
func (r AgentRules) Asks() bool { return r.AskSeveral == nil || *r.AskSeveral }

// set reports whether the rules set anything, which is a section.
func (r AgentRules) set() bool { return r.ChangeTickets != nil || r.AskSeveral != nil }

// TimeSettings is where a project's time goes: the ticket meeting hours
// go on and the ticket time off goes on, each a Jira ticket, and whether
// an agent's time draft may put hours on an epic, which it may not
// unless NeverEpics is false. A person's own log is not held to it.
type TimeSettings struct {
	Meetings   *TimeTicket `json:"meetings,omitempty"`
	TimeOff    *TimeTicket `json:"time_off,omitempty"`
	NeverEpics *bool       `json:"never_epics,omitempty"`
}

// RefusesEpics is whether an agent's draft may not put hours on an epic.
func (t TimeSettings) RefusesEpics() bool { return t.NeverEpics == nil || *t.NeverEpics }

// Estimates is how a project counts the work of its Jira boards, and
// whether its Jira tickets offer planning poker. Count is "tickets" when
// the project counts tickets in place of the boards' estimates, as a
// board with no estimates is counted, and absent for the boards'
// estimates; Poker is false when the project turned planning poker off.
// Only a Jira board is counted by it: a project that lists none keeps
// the section for when it lists one.
type Estimates struct {
	Count string `json:"count,omitempty"`
	Poker *bool  `json:"poker,omitempty"`
}

// estimatesCountTickets is Estimates.Count for counting tickets.
const estimatesCountTickets = "tickets"

// countsTickets is whether the project counts its Jira boards' tickets.
func (e *Estimates) countsTickets() bool { return e != nil && e.Count == estimatesCountTickets }

// Credit is how a project counts credit for the work beside a ticket's
// own points: whether it counts it (On; off keeps the rules and the
// support picks), the rules it counts it by (Rules), and which tickets
// are support (Support). Rules is a list of whole snapshots, each in
// force from its From until the next one's: a closed sprint counts by
// the snapshot in force when it completed, anything else by the newest.
// Support is a classification, not a rule of a sprint: a change of it
// applies to every sprint. A project with no section counts no credit
// and reads as off with the default rules (defaultCredit); a section
// that says no more than that is stored as none. Only a Jira board is
// counted by it, as by Estimates.
type Credit struct {
	On      bool          `json:"on"`
	Rules   []CreditRules `json:"rules"`
	Support []SiteSupport `json:"support"`
}

// CreditRules is one snapshot of a project's credit rules: From, when it
// took effect (Unix milliseconds; the first is 0), and its values. A
// reviewer of a finished ticket who is not its assignee and did at least
// ReviewNeed different kinds of ReviewActions (commented, approved,
// logged_time) on it gets ReviewShare percent of its points; hours logged
// on a support ticket count as a point for every HoursPerPoint hours (a
// ticket for every HoursPerPoint hours where the project counts tickets).
// A replace's newest snapshot may leave From out, which means from this
// save; the writer stamps it (stampCredit).
type CreditRules struct {
	From          *int64   `json:"from,omitempty"`
	ReviewShare   int      `json:"review_share"`
	ReviewNeed    int      `json:"review_need"`
	ReviewActions []string `json:"review_actions"`
	HoursPerPoint int      `json:"hours_per_point"`
}

// SiteSupport is which tickets of one Jira site are support for the
// project: those of an issue type (by ID), with a label (by name) or on a
// board (by ID) it lists. A ticket is support when it has no estimate and
// the entry names it, so on a board that does not estimate every ticket
// it names is (store mirrorSupportSQL). A support ticket is not counted
// as a ticket without an estimate, and its logged hours count as credit.
type SiteSupport struct {
	Tracker string   `json:"tracker"`
	Site    string   `json:"site"`
	Types   []string `json:"types"`
	Labels  []string `json:"labels"`
	Boards  []string `json:"boards"`
}

// creditActions are the review actions a credit rule may count, in the
// order a rule holds them.
var creditActions = []string{"commented", "approved", "logged_time"}

// The bounds of a credit rule's values, and of a support entry's lists.
const (
	creditMaxShare         = 100
	creditMaxHoursPerPoint = 16
	creditMaxSupport       = 100
)

// defaultCredit is what a project with no credit section reads as: off,
// with one snapshot from the start, 10% of a finished ticket's points to
// each reviewer who did at least 2 of the three actions, and a point for
// every 4 hours of support.
func defaultCredit() *Credit {
	from := int64(0)
	return &Credit{Rules: []CreditRules{{From: &from, ReviewShare: 10, ReviewNeed: 2, ReviewActions: slices.Clone(creditActions), HoursPerPoint: 4}},
		Support: []SiteSupport{}}
}

// TimeTicket is a ticket a project's time goes on, by its site and key.
type TimeTicket struct {
	Tracker string `json:"tracker"`
	Site    string `json:"site"`
	Key     string `json:"key"`
}

// SiteName is the ticket's site as a TicketSource names it.
func (t TimeTicket) SiteName() string { return siteName(t.Site) }

// SetupView is a project's tracker setup as it is stored: the revision a
// replace names (empty while there is no file), the setup itself, which a
// replace gives back as it is or changed, and the names of the sections a
// newer version saved, which this build keeps as they are. The setup is a
// field of its own, beside what a replace does not give, so the value a
// screen edits and saves is the value it read.
type SetupView struct {
	Revision string   `json:"revision"`
	Setup    Setup    `json:"setup"`
	Newer    []string `json:"newer,omitempty"`
}

// SetupSave is a replace's answer: whether it saved, the sentence that
// says why not when it did not (the stored setup moved since the revision
// it named), and the setup as it is stored now either way.
type SetupSave struct {
	Saved   bool      `json:"saved"`
	Refused string    `json:"refused,omitempty"`
	Stored  SetupView `json:"stored"`
}

// setupChangedSentence is a refused replace's sentence.
const setupChangedSentence = "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now"

// StatusGroups is how a project counts the rows of one tracker site's
// boards, as a board read applies it (store.MirrorRegroup): its
// regrouping of the site's statuses and, while the project counts credit,
// its support tickets (SiteSupport), with the version of both: a board's
// view, its section cursors and the totals a patch names carry the
// version, and a read under another version is refused (ErrStalePage).
// statuses is the site's status groups entry, each regrouped status's
// group by name, which a board view carries so that a client counting
// rows it holds counts each in the group the store's totals do; a row's
// support it reads from the row (RowView.Support). All of them come from
// the site's entries of the setup, so they change together. The zero
// value regroups nothing and names no row support, at version "".
// tickets is whether the project counts the site's boards by their
// tickets (Estimates), which a board view says as its unit of counts
// (unit) and a dashboard counts by; the store's totals carry both rows
// and estimates, so it is not part of the version.
type StatusGroups struct {
	regroup  store.MirrorRegroup
	version  string
	statuses map[string]string
	tickets  bool
}

// unit is the unit the project counts a board of the site whose
// estimates are in board in: the board's own, or none when the project
// counts tickets.
func (g StatusGroups) unit(board tracker.EstimateUnit) tracker.EstimateUnit {
	if g.tickets {
		return tracker.UnitNone
	}
	return board
}

// setupGroup is the store's number of the group named name, which a
// status may be put in, and false for any other name.
func setupGroup(name string) (int, bool) {
	switch name {
	case "backlog":
		return store.MirrorGroupBacklog, true
	case "todo":
		return store.MirrorGroupToDo, true
	case "in_progress":
		return store.MirrorGroupInProgress, true
	case "done":
		return store.MirrorGroupDone, true
	case "canceled":
		return store.MirrorGroupCanceled, true
	case "left_out":
		return store.MirrorGroupLeftOut, true
	}
	return 0, false
}

// siteKey names a tracker site: its kind and site, as a connection
// names them. A setup entry names a site, not a connection, so it
// survives a connection removed and added again.
type siteKey struct {
	tracker, site string
}

// setupKey names the status groups a board handle reads with: its
// project's slug and its connection's site.
type setupKey struct {
	slug string
	site siteKey
}

// heldSetup is a project's setup as the service holds it.
type heldSetup struct {
	view SetupView
	// raw is every section the file holds that this build does not read,
	// by name, as its bytes.
	raw map[string]json.RawMessage
	// dashboards is the dashboards section, which a replace keeps.
	dashboards []BoardDashboards
	// groups is each site's status groups.
	groups map[siteKey]StatusGroups
}

// ProjectSetup is projectID's tracker setup as it is stored.
func (s *Service) ProjectSetup(projectID string) (SetupView, error) {
	if err := s.enter(); err != nil {
		return SetupView{}, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return SetupView{}, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(slug)
	if err != nil {
		return SetupView{}, err
	}
	return held.view, nil
}

// ProjectAgentRules is what projectID's setup lets a thread of it do to
// tickets. A setup file that cannot be read fails as ProjectSetup does,
// and an agents section a newer version saved fails with ErrSetupNewer:
// this build cannot tell what it allows, so its caller refuses rather
// than apply the built-in rules.
func (s *Service) ProjectAgentRules(projectID string) (AgentRules, error) {
	setup, err := s.projectSection(projectID, sectionAgents)
	if err != nil || setup.Agents == nil {
		return AgentRules{}, err
	}
	return *setup.Agents, nil
}

// ProjectTimeSettings is where projectID's setup puts its time, as
// ProjectAgentRules reads what it allows: a time section a newer version
// saved fails with ErrSetupNewer. Time settings are a Jira project's: a
// project that lists no board of a Jira connection has the built-in ones,
// and the setup keeps what it holds for when one is listed again, as the
// Time page shows them (TrackerTimeTickets.svelte).
func (s *Service) ProjectTimeSettings(projectID string) (TimeSettings, error) {
	boards, err := s.ProjectBoards(projectID)
	if err != nil {
		return TimeSettings{}, err
	}
	if !slices.ContainsFunc(boards, func(b BoardChoice) bool { return b.ConnectionKind == store.TrackerKindJira }) {
		return TimeSettings{}, nil
	}
	setup, err := s.projectSection(projectID, sectionTime)
	if err != nil || setup.Time == nil {
		return TimeSettings{}, err
	}
	return *setup.Time, nil
}

// projectSection is projectID's setup for a reader of section, which
// fails with ErrSetupNewer when a newer version saved that section.
func (s *Service) projectSection(projectID, section string) (Setup, error) {
	view, err := s.ProjectSetup(projectID)
	if err != nil {
		return Setup{}, err
	}
	if slices.Contains(view.Newer, section) {
		return Setup{}, fmt.Errorf("%w: section %s", ErrSetupNewer, section)
	}
	return view.Setup, nil
}

// SetProjectSetup replaces projectID's tracker setup with setup, built
// from the stored setup at revision. When the stored revision is another,
// nothing is written and the answer says so with the setup stored now.
// A setup that does not validate fails with ErrInvalidSetup, a stored
// file that cannot be read with ErrSetupUnreadable (it is not
// overwritten), and a change to a section a newer version saved with
// ErrSetupNewer, and a write or removal of the file that failed with
// ErrSetupWriteUnsure (the file may hold the change or not). An empty
// setup removes the file, unless it keeps a section a newer version
// saved. What the save saw of the file is what the service holds after
// it, refused or failed (replaceSetup), and who is told follows from that
// (setupMoved).
func (s *Service) SetProjectSetup(projectID, revision string, setup Setup) (SetupSave, error) {
	if err := s.enter(); err != nil {
		return SetupSave{}, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return SetupSave{}, err
	}
	if setup, err = s.stampCredit(projectID, setup); err != nil {
		return SetupSave{}, err
	}
	setup, err = validateSetup(setup)
	if err != nil {
		return SetupSave{}, err
	}
	save, before, after, err := s.replaceSetup(slug, revision, setup)
	s.setupMoved(slug, before, after)
	if err != nil {
		return SetupSave{}, err
	}
	return save, nil
}

// stampCredit is setup with its credit section's newest snapshot given
// the time it takes effect when the replace leaves it out (CreditRules):
// the start for a section's first snapshot. A later one replaces the
// snapshot before it, and takes its time, while no sprint of the
// project's Jira boards the mirror holds completed after that one took
// effect, so no closed sprint was counted by it; otherwise it takes
// effect from this save. So the list grows by at most one snapshot a
// sprint, and a project whose boards close no sprint keeps one. Any other
// snapshot without its time is left for checkCredit to refuse.
func (s *Service) stampCredit(projectID string, setup Setup) (Setup, error) {
	c := setup.Credit
	if c == nil || len(c.Rules) == 0 || c.Rules[len(c.Rules)-1].From != nil {
		return setup, nil
	}
	credit := *c
	credit.Rules = slices.Clone(c.Rules)
	setup.Credit = &credit
	newest := len(credit.Rules) - 1
	if newest == 0 {
		credit.Rules[0].From = new(int64(0))
		return setup, nil
	}
	before := credit.Rules[newest-1].From
	if before == nil {
		return setup, nil
	}
	completed, err := s.lastSprintCompleted(projectID)
	if err != nil {
		return Setup{}, err
	}
	if now := s.now().UnixMilli(); completed > *before && now > *before {
		credit.Rules[newest].From = &now
		return setup, nil
	}
	credit.Rules[newest].From = before
	credit.Rules = slices.Delete(credit.Rules, newest-1, newest)
	return setup, nil
}

// lastSprintCompleted is when the last sprint of projectID's Jira boards
// that the mirror holds completed (Unix milliseconds), 0 when none did.
// The caller is inside the service (enter).
func (s *Service) lastSprintCompleted(projectID string) (int64, error) {
	choices, err := s.boardChoices(projectID)
	if err != nil {
		return 0, err
	}
	var last int64
	roots := map[string]int64{}
	for _, choice := range choices {
		if choice.ConnectionKind != store.TrackerKindJira {
			continue
		}
		// A root is never 0, which stands for a connection with none.
		root, seen := roots[choice.Connection]
		if !seen {
			if root, _, err = s.store.GetMirrorRoot(choice.Connection); err != nil {
				return 0, fmt.Errorf("trackerapp: the mirror of connection %s: %w", choice.Connection, err)
			}
			roots[choice.Connection] = root
		}
		if root == 0 {
			continue
		}
		st, ok, err := s.store.GetMirrorBoard(root, choice.Board)
		if err != nil {
			return 0, fmt.Errorf("trackerapp: read board %s of connection %s: %w", choice.Board, choice.Connection, err)
		}
		if !ok {
			continue
		}
		for _, sp := range st.Sprints {
			if sp.State == store.MirrorSprintClosed {
				last = max(last, sp.CompletedAt)
			}
		}
	}
	return last, nil
}

// replaceSetup does SetProjectSetup's work under setupMu, comparing the
// revision with the file's bytes, and returns the held setup before and
// after, each nil when none is held. After is what the save last saw of
// the file, which the service holds when it returns, whatever it returns:
// the file it read, or the setup it wrote; nil when it could not read the
// file, or its write may or may not have happened, so the next use reads
// the file.
func (s *Service) replaceSetup(slug, revision string, setup Setup) (save SetupSave, before, after *heldSetup, err error) {
	moved := false
	before, after, err = s.rewriteSetup(slug, func(stored *heldSetup) (Setup, []BoardDashboards, bool) {
		moved = stored.view.Revision != revision
		return setup, stored.dashboards, !moved
	})
	switch {
	case err != nil:
		return SetupSave{}, before, after, err
	case moved:
		return SetupSave{Refused: setupChangedSentence, Stored: after.view}, before, after, nil
	}
	return SetupSave{Saved: true, Stored: after.view}, before, after, nil
}

// rewriteSetup reads slug's file again under setupMu and writes what
// change makes of what it holds: the setup and the dashboards, both
// valid, or nothing when change answers false. It returns the held setup
// before and after, each nil when none is held. After is what the write
// last saw of the file, which the service holds when it returns, whatever
// it returns: the file it read, or the setup it wrote; nil when it could
// not read the file, or its write may or may not have happened, so the
// next use reads the file.
func (s *Service) rewriteSetup(slug string, change func(stored *heldSetup) (Setup, []BoardDashboards, bool)) (before, after *heldSetup, err error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	before = s.setups[slug]
	defer func() {
		if after == nil {
			delete(s.setups, slug)
		} else {
			s.setups[slug] = after
		}
	}()
	path := s.setupPath(slug)
	if after, err = readSetupFile(path); err != nil {
		return before, nil, err
	}
	setup, dashboards, write := change(after)
	if !write {
		return before, after, nil
	}
	data, err := encodeSetup(setup, dashboards, after.raw, after.view.Newer)
	if err != nil {
		return before, after, err
	}
	written, err := heldOf(setup, dashboards, after.raw, after.view.Newer, setupRevision(data))
	if err != nil {
		return before, after, err
	}
	if data == nil {
		err = atomicfile.Remove(path)
	} else {
		err = atomicfile.Write(path, data)
	}
	if err != nil {
		return before, nil, &SetupWriteError{Path: path, Err: err}
	}
	return before, written, nil
}

// setupMoved tells what a change of slug's held setup from before to
// after moves, either nil when none is held: every board subscription of
// the project on a site whose status groups, support tickets (both in the
// version) or count of tickets changed reads again, a change of the count has the dashboards read again, and
// a change of what is stored is told. A side that holds nothing is not
// known to agree with the other on any site, so every site counts as
// moved: a board of the project read with groups that may be stale, or
// failed to read them, reads again.
func (s *Service) setupMoved(slug string, before, after *heldSetup) {
	switch {
	case before == nil && after == nil:
		return
	case before != nil && after != nil && before.view.Revision == after.view.Revision:
		return
	}
	s.readBoardsAgain(slug, func(site siteKey) bool {
		if before == nil || after == nil {
			return true
		}
		was, now := before.counting(site), after.counting(site)
		return was.version != now.version || was.tickets != now.tickets
	})
	// A board's dashboards are laid out in the unit the project counts it
	// in (Dashboards): a change of it has them read again.
	if before == nil || after == nil || before.view.Setup.Estimates.countsTickets() != after.view.Setup.Estimates.countsTickets() {
		s.dashboardsChanged()
	}
	s.setupChanged()
}

// readBoardsAgain has every board pump with a handle of project slug on a
// site moved holds for send ReadAgain.
func (s *Service) readBoardsAgain(slug string, moved func(siteKey) bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	told := map[*boardPump]bool{}
	for _, h := range s.subs.handles {
		p := h.board
		if p == nil || told[p] || h.setup.slug != slug || !moved(h.setup.site) || p.over || p.core.DeadLocked() {
			continue
		}
		told[p] = true
		p.pending.add(nil, nil, true)
		s.flushBoardLocked(p)
	}
}

// boardStatusGroups is the status groups key's project gives its site,
// loading the project's setup when none is held.
func (s *Service) boardStatusGroups(key setupKey) (StatusGroups, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(key.slug)
	if err != nil {
		return StatusGroups{}, err
	}
	return held.counting(key.site), nil
}

// counting is how the setup counts the boards of site: its status
// groups, and on a Jira site whether it counts tickets.
func (h *heldSetup) counting(site siteKey) StatusGroups {
	g := h.groups[site]
	g.tickets = site.tracker == store.TrackerKindJira && h.view.Setup.Estimates.countsTickets()
	return g
}

// heldSetupLocked is slug's held setup, read from its file when none is
// held; a file that cannot be read is not held. The caller holds setupMu.
func (s *Service) heldSetupLocked(slug string) (*heldSetup, error) {
	if held := s.setups[slug]; held != nil {
		return held, nil
	}
	held, err := readSetupFile(s.setupPath(slug))
	if err != nil {
		return nil, err
	}
	s.setups[slug] = held
	return held, nil
}

// projectSlug is projectID's slug, which names its app directory.
func (s *Service) projectSlug(projectID string) (string, error) {
	if projectID == "" {
		return "", fmt.Errorf("%w: a project's tracker setup names the project", ErrProjectNotFound)
	}
	p, err := s.store.GetProject(projectID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s: %w", ErrProjectNotFound, projectID, err)
	case err != nil:
		return "", fmt.Errorf("trackerapp: read project %s: %w", projectID, err)
	}
	return p.Slug, nil
}

// setupPath is the setup file of the project slug names.
func (s *Service) setupPath(slug string) string {
	return filepath.Join(project.ConfigDir(s.configDir, slug), setupFileName)
}

// readSetupFile reads and validates the setup file at path; no file is no
// setup.
func readSetupFile(path string) (*heldSetup, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return heldOf(Setup{}, nil, nil, nil, "")
	}
	if err != nil {
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	}
	data, err := io.ReadAll(io.LimitReader(f, setupMaxBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	case len(data) > setupMaxBytes:
		return nil, &SetupFileError{Path: path, Reason: fmt.Sprintf("it is over %d KiB", setupMaxBytes>>10)}
	}
	held, err := decodeSetup(data)
	if err != nil {
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	}
	return held, nil
}

// setupSection is a section's version, which a section of any version
// holds.
type setupSection struct {
	Version int `json:"version"`
}

// The sections this build reads, at the version it writes.
type (
	statusGroupsV1 struct {
		Version int                `json:"version"`
		Sites   []SiteStatusGroups `json:"sites"`
	}
	ticketPinsV1 struct {
		Version int        `json:"version"`
		Sites   []SitePins `json:"sites"`
	}
	startWorkV1 struct {
		Version  int                 `json:"version"`
		Projects []StartWorkDefaults `json:"projects"`
	}
	agentsV1 struct {
		Version       int   `json:"version"`
		ChangeTickets *bool `json:"change_tickets,omitempty"`
		AskSeveral    *bool `json:"ask_several,omitempty"`
	}
	timeV1 struct {
		Version    int         `json:"version"`
		Meetings   *TimeTicket `json:"meetings,omitempty"`
		TimeOff    *TimeTicket `json:"time_off,omitempty"`
		NeverEpics *bool       `json:"never_epics,omitempty"`
	}
	estimatesV1 struct {
		Version int    `json:"version"`
		Count   string `json:"count,omitempty"`
		Poker   *bool  `json:"poker,omitempty"`
	}
	creditV1 struct {
		Version int           `json:"version"`
		On      bool          `json:"on"`
		Rules   []CreditRules `json:"rules"`
		Support []SiteSupport `json:"support,omitempty"`
	}
)

// decodeSetup is the setup data holds, every section this build does not
// read kept as its bytes.
func decodeSetup(data []byte) (*heldSetup, error) {
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		return nil, fmt.Errorf("it is not a JSON object of sections: %w", err)
	}
	if sections == nil {
		return nil, errors.New("it is not a JSON object of sections")
	}
	var setup Setup
	var dashboards []BoardDashboards
	var raw map[string]json.RawMessage
	var newer []string
	keep := func(name string, section json.RawMessage) {
		if raw == nil {
			raw = map[string]json.RawMessage{}
		}
		raw[name], newer = section, append(newer, name)
	}
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		section := sections[name]
		if !slices.Contains([]string{sectionStatusGroups, sectionTicketPins, sectionStartWork, sectionAgents, sectionTime, sectionEstimates, sectionCredit, sectionDashboards}, name) {
			keep(name, section)
			continue
		}
		var v setupSection
		if err := json.Unmarshal(section, &v); err != nil {
			return nil, fmt.Errorf("section %s: %w", name, err)
		}
		switch {
		case v.Version > setupSectionVersion:
			keep(name, section)
			continue
		case v.Version < setupSectionVersion:
			return nil, fmt.Errorf("section %s has no version this build reads (version %d)", name, v.Version)
		}
		var err error
		switch name {
		case sectionStatusGroups:
			var s statusGroupsV1
			err = decodeStrict(section, &s)
			setup.StatusGroups = s.Sites
		case sectionTicketPins:
			var s ticketPinsV1
			err = decodeStrict(section, &s)
			setup.TicketPins = s.Sites
		case sectionStartWork:
			var s startWorkV1
			err = decodeStrict(section, &s)
			setup.StartWork = s.Projects
		case sectionAgents:
			var s agentsV1
			err = decodeStrict(section, &s)
			setup.Agents = &AgentRules{ChangeTickets: s.ChangeTickets, AskSeveral: s.AskSeveral}
		case sectionTime:
			var s timeV1
			err = decodeStrict(section, &s)
			setup.Time = &TimeSettings{Meetings: s.Meetings, TimeOff: s.TimeOff, NeverEpics: s.NeverEpics}
		case sectionEstimates:
			var s estimatesV1
			err = decodeStrict(section, &s)
			setup.Estimates = &Estimates{Count: s.Count, Poker: s.Poker}
		case sectionCredit:
			var s creditV1
			err = decodeStrict(section, &s)
			setup.Credit = &Credit{On: s.On, Rules: s.Rules, Support: s.Support}
		case sectionDashboards:
			var s dashboardsV1
			if err = decodeStrict(section, &s); err == nil {
				dashboards, err = validateDashboards(s.Boards)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("section %s: %w", name, err)
		}
	}
	valid, err := validateSetup(setup)
	if err != nil {
		var invalid *SetupError
		if errors.As(err, &invalid) {
			return nil, errors.New(invalid.Reason)
		}
		return nil, err
	}
	return heldOf(valid, dashboards, raw, newer, setupRevision(data))
}

// decodeStrict decodes data into v, refusing a field v does not have.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// encodeSetup is the file of setup, the dashboards and the sections raw
// keeps: a JSON object of the sections in name order, each kept section
// as its bytes; nil when it holds no section, which is no file. A section
// that would replace one a newer version saved (newer, SetupView.Newer)
// is ErrSetupNewer: this is the one place a section is written, so every
// section, one added later included, keeps that rule.
func encodeSetup(setup Setup, dashboards []BoardDashboards, raw map[string]json.RawMessage, newer []string) ([]byte, error) {
	sections := map[string]json.RawMessage{}
	for name, section := range raw {
		sections[name] = section
	}
	add := func(name string, v any) error {
		if slices.Contains(newer, name) {
			return fmt.Errorf("%w: section %s", ErrSetupNewer, name)
		}
		encoded, err := json.MarshalIndent(v, "  ", "  ")
		if err != nil {
			return fmt.Errorf("trackerapp: encode the tracker setup's %s: %w", name, err)
		}
		sections[name] = encoded
		return nil
	}
	if len(setup.StatusGroups) > 0 {
		if err := add(sectionStatusGroups, statusGroupsV1{Version: setupSectionVersion, Sites: setup.StatusGroups}); err != nil {
			return nil, err
		}
	}
	if len(setup.TicketPins) > 0 {
		if err := add(sectionTicketPins, ticketPinsV1{Version: setupSectionVersion, Sites: setup.TicketPins}); err != nil {
			return nil, err
		}
	}
	if len(setup.StartWork) > 0 {
		if err := add(sectionStartWork, startWorkV1{Version: setupSectionVersion, Projects: setup.StartWork}); err != nil {
			return nil, err
		}
	}
	if setup.Agents != nil {
		if err := add(sectionAgents, agentsV1{Version: setupSectionVersion, ChangeTickets: setup.Agents.ChangeTickets, AskSeveral: setup.Agents.AskSeveral}); err != nil {
			return nil, err
		}
	}
	if setup.Time != nil {
		if err := add(sectionTime, timeV1{Version: setupSectionVersion, Meetings: setup.Time.Meetings, TimeOff: setup.Time.TimeOff, NeverEpics: setup.Time.NeverEpics}); err != nil {
			return nil, err
		}
	}
	if setup.Estimates != nil {
		if err := add(sectionEstimates, estimatesV1{Version: setupSectionVersion, Count: setup.Estimates.Count, Poker: setup.Estimates.Poker}); err != nil {
			return nil, err
		}
	}
	if setup.Credit.stored() {
		if err := add(sectionCredit, creditV1{Version: setupSectionVersion, On: setup.Credit.On, Rules: setup.Credit.Rules, Support: setup.Credit.Support}); err != nil {
			return nil, err
		}
	}
	if len(dashboards) > 0 {
		if err := add(sectionDashboards, dashboardsV1{Version: setupSectionVersion, Boards: dashboards}); err != nil {
			return nil, err
		}
	}
	if len(sections) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	out.WriteString("{\n")
	for i, name := range slices.Sorted(maps.Keys(sections)) {
		if i > 0 {
			out.WriteString(",\n")
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: encode the tracker setup's section name %q: %w", name, err)
		}
		out.WriteString("  ")
		out.Write(key)
		out.WriteString(": ")
		out.Write(sections[name])
	}
	out.WriteString("\n}\n")
	if out.Len() > setupMaxBytes {
		return nil, &SetupError{Reason: fmt.Sprintf("the setup is over %d KiB", setupMaxBytes>>10), Full: true}
	}
	return out.Bytes(), nil
}

// setupRevision is the revision of a file of data: its SHA-256, empty
// for no file.
func setupRevision(data []byte) string {
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// heldOf is a valid setup and valid dashboards as the service holds
// them, with the sections raw keeps (named by newer) and the revision of
// its file.
// A section a newer version saved reads as none here; a credit section
// that is none reads as the default (defaultCredit), whose controls the
// page shows, unless a newer version saved it.
func heldOf(setup Setup, dashboards []BoardDashboards, raw map[string]json.RawMessage, newer []string, revision string) (*heldSetup, error) {
	credit := setup.Credit
	if credit == nil && !slices.Contains(newer, sectionCredit) {
		credit = defaultCredit()
	}
	held := &heldSetup{
		view: SetupView{Revision: revision, Setup: Setup{StatusGroups: slicesx.OrEmpty(setup.StatusGroups), TicketPins: slicesx.OrEmpty(setup.TicketPins),
			StartWork: slicesx.OrEmpty(setup.StartWork), Agents: setup.Agents, Time: setup.Time, Estimates: setup.Estimates, Credit: credit}, Newer: newer},
		raw: raw, dashboards: dashboards, groups: map[siteKey]StatusGroups{},
	}
	// What a board read applies on each site: its status groups, and its
	// support entry while the project counts credit.
	type counted struct {
		statuses map[string]string
		support  *SiteSupport
	}
	sites := map[siteKey]counted{}
	for _, site := range setup.StatusGroups {
		key := siteKey{site.Tracker, site.Site}
		sites[key] = counted{statuses: site.Statuses}
	}
	if c := held.credit(); c != nil {
		for i, site := range c.Support {
			key := siteKey{site.Tracker, site.Site}
			entry := sites[key]
			entry.support = &c.Support[i]
			sites[key] = entry
		}
	}
	for key, site := range sites {
		groups := make(map[string]int, len(site.statuses))
		for id, name := range site.statuses {
			groups[id], _ = setupGroup(name)
		}
		regroup, err := store.NewMirrorRegroup(groups)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the status groups of %s %s: %w", key.tracker, key.site, err)
		}
		if site.support != nil {
			rule := store.MirrorSupportRule{Types: site.support.Types, Labels: site.support.Labels, Boards: site.support.Boards}
			if regroup, err = regroup.WithSupport(rule); err != nil {
				return nil, fmt.Errorf("trackerapp: the support tickets of %s %s: %w", key.tracker, key.site, err)
			}
		}
		// json.Marshal writes a map's keys in order, so equal groups and
		// support have one version.
		encoded, err := json.Marshal(struct {
			Statuses map[string]string `json:"statuses"`
			Support  *SiteSupport      `json:"support"`
		}{site.statuses, site.support})
		if err != nil {
			return nil, fmt.Errorf("trackerapp: encode how %s %s is counted: %w", key.tracker, key.site, err)
		}
		sum := sha256.Sum256(encoded)
		held.groups[key] = StatusGroups{regroup: regroup, version: hex.EncodeToString(sum[:8]), statuses: site.statuses}
	}
	return held, nil
}

// credit is the project's credit section when the project counts credit,
// and nil when it does not: it has no section, or one that is off, or
// one a newer version saved, which this build cannot read and so holds no
// section for (heldOf). The one answer
// to whether a project counts credit, for the counts, the reads and the
// page.
func (h *heldSetup) credit() *Credit {
	if c := h.view.Setup.Credit; c != nil && c.On {
		return c
	}
	return nil
}

// validateSetup checks setup and returns it normalized: each site as its
// connection names it (checkSite), each section's sites in (tracker,
// site) order, without a site that sets nothing.
func validateSetup(setup Setup) (Setup, error) {
	var out Setup
	seen := map[siteKey]bool{}
	for _, site := range setup.StatusGroups {
		key, err := checkSite(site.Tracker, site.Site, sectionStatusGroups, seen)
		if err != nil {
			return Setup{}, err
		}
		site.Site = key.site
		if len(site.Statuses) > setupMaxStatuses {
			return Setup{}, invalidSetup("%s %s regroups %d statuses, over %d", key.tracker, key.site, len(site.Statuses), setupMaxStatuses)
		}
		for id, group := range site.Statuses {
			if err := checkSetupID("status", id); err != nil {
				return Setup{}, err
			}
			if _, ok := setupGroup(group); !ok {
				return Setup{}, invalidSetup("status %s of %s %s is put in group %q, which is none of backlog, todo, in_progress, done, canceled or left_out",
					id, key.tracker, key.site, group)
			}
		}
		if len(site.Statuses) > 0 {
			out.StatusGroups = append(out.StatusGroups, site)
		}
	}
	clear(seen)
	for _, site := range setup.TicketPins {
		key, err := checkSite(site.Tracker, site.Site, sectionTicketPins, seen)
		if err != nil {
			return Setup{}, err
		}
		site.Site = key.site
		if site.Fields != nil && key.tracker == store.TrackerKindLinear {
			return Setup{}, invalidSetup("linear %s pins fields, but a Linear ticket's fields are fixed", key.site)
		}
		if err := checkSetupList(key, "pins", "field", site.Fields, setupMaxFields); err != nil {
			return Setup{}, err
		}
		if err := checkSetupList(key, "pins", "section", site.Sections, setupMaxSections); err != nil {
			return Setup{}, err
		}
		// No field pinned is a ticket's default, and is stored as none; no
		// section open is a choice of its own.
		if site.Fields != nil && len(*site.Fields) == 0 {
			site.Fields = nil
		}
		if site.Fields != nil || site.Sections != nil {
			out.TicketPins = append(out.TicketPins, site)
		}
	}
	if len(setup.StartWork) > setupMaxStartWork {
		return Setup{}, invalidSetup("%s has %d entries, over %d", sectionStartWork, len(setup.StartWork), setupMaxStartWork)
	}
	seenProjects := map[startWorkKey]bool{}
	for _, entry := range setup.StartWork {
		valid, set, err := checkStartWork(entry, seenProjects)
		if err != nil {
			return Setup{}, err
		}
		if set {
			out.StartWork = append(out.StartWork, valid)
		}
	}
	if setup.Agents != nil {
		// Each rule is on by default: only its off is stored, and a
		// section that turns nothing off is none.
		rules := AgentRules{}
		if setup.Agents.ChangeTickets != nil && !*setup.Agents.ChangeTickets {
			rules.ChangeTickets = setup.Agents.ChangeTickets
		}
		if setup.Agents.AskSeveral != nil && !*setup.Agents.AskSeveral {
			rules.AskSeveral = setup.Agents.AskSeveral
		}
		if rules.set() {
			out.Agents = &rules
		}
	}
	if setup.Time != nil {
		settings, err := checkTime(*setup.Time)
		if err != nil {
			return Setup{}, err
		}
		out.Time = settings
	}
	if setup.Estimates != nil {
		estimates, err := checkEstimates(*setup.Estimates)
		if err != nil {
			return Setup{}, err
		}
		out.Estimates = estimates
	}
	if setup.Credit != nil {
		credit, err := checkCredit(*setup.Credit)
		if err != nil {
			return Setup{}, err
		}
		out.Credit = credit
	}
	slices.SortFunc(out.StartWork, func(a, b StartWorkDefaults) int {
		if c := compareSites(a.Tracker, a.Site, b.Tracker, b.Site); c != 0 {
			return c
		}
		return strings.Compare(a.Project, b.Project)
	})
	slices.SortFunc(out.StatusGroups, func(a, b SiteStatusGroups) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	slices.SortFunc(out.TicketPins, func(a, b SitePins) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	return out, nil
}

// checkSite checks a section's entry's tracker and site and returns the
// site as a connection of the tracker names it, once each in the section
// (seen). It keeps three things true:
//   - the site it returns is the connection's rule's (jiraSite,
//     linearSite): the value stored and the key a board handle looks up,
//     so an entry applies however its site is spelled;
//   - every bound applies to that value, so checking a checked setup
//     returns it unchanged, and whatever a save writes the reader of the
//     same build accepts;
//   - a refusal names the section, the tracker and the rule's reason,
//     never the text given for the site, which may hold a credential (a
//     pasted address's user info or token); a sentence quotes only a value
//     the rule returned, which holds none.
func checkSite(tracker, site, section string, seen map[siteKey]bool) (siteKey, error) {
	if tracker != store.TrackerKindJira && tracker != store.TrackerKindLinear {
		return siteKey{}, invalidSetup("%s names tracker %q, which is neither jira nor linear", section, tracker)
	}
	if strings.TrimSpace(site) == "" {
		return siteKey{}, invalidSetup("%s names an empty %s site", section, tracker)
	}
	named := jiraSite
	if tracker == store.TrackerKindLinear {
		named = linearSite
	}
	canonical, err := named(site)
	if err != nil {
		reason := "it is not one"
		if refused, ok := errors.AsType[*InvalidError](err); ok {
			reason = refused.Reason
		}
		return siteKey{}, invalidSetup("%s names a %s site its connection could not use: %s", section, tracker, reason)
	}
	if len(canonical) > setupMaxID {
		return siteKey{}, invalidSetup("%s names a %s site over %d bytes as its connection names it", section, tracker, setupMaxID)
	}
	key := siteKey{tracker, canonical}
	if seen[key] {
		return siteKey{}, invalidSetup("%s names %s %s twice", section, tracker, canonical)
	}
	seen[key] = true
	return key, nil
}

// startWorkKey names a start_work entry: its site and tracker project.
type startWorkKey struct {
	site    siteKey
	project string
}

// checkStartWork checks a start_work entry and returns it with its site as
// its connection names it; set is false for an entry that sets nothing.
// The tracker project is a key's prefix (TicketKeys), once per site; a
// branch template is at most setupMaxBranchTemplate bytes and holds no
// token but {key} and {title}.
func checkStartWork(entry StartWorkDefaults, seen map[startWorkKey]bool) (StartWorkDefaults, bool, error) {
	key, err := checkSite(entry.Tracker, entry.Site, sectionStartWork, map[siteKey]bool{})
	if err != nil {
		return StartWorkDefaults{}, false, err
	}
	entry.Site = key.site
	if !isTicketProject(entry.Project) {
		return StartWorkDefaults{}, false, invalidSetup("%s names tracker project %q of %s %s, which is not a key's prefix (WEB of WEB-1)",
			sectionStartWork, truncateID(entry.Project), key.tracker, key.site)
	}
	project := startWorkKey{key, entry.Project}
	if seen[project] {
		return StartWorkDefaults{}, false, invalidSetup("%s names %s of %s %s twice", sectionStartWork, entry.Project, key.tracker, key.site)
	}
	seen[project] = true
	if entry.Status != "" {
		if err := checkSetupID("status", entry.Status); err != nil {
			return StartWorkDefaults{}, false, err
		}
	}
	if entry.Repository != "" {
		if err := checkSetupID("repository", entry.Repository); err != nil {
			return StartWorkDefaults{}, false, err
		}
	}
	if len(entry.Branch) > setupMaxBranchTemplate {
		return StartWorkDefaults{}, false, invalidSetup("the branch template of %s is over %d bytes", entry.Project, setupMaxBranchTemplate)
	}
	rest := strings.NewReplacer("{key}", "", "{title}", "").Replace(entry.Branch)
	if strings.ContainsAny(rest, "{}") {
		return StartWorkDefaults{}, false, invalidSetup("the branch template of %s is %q; its only tokens are {key} and {title}", entry.Project, entry.Branch)
	}
	// Naming the branch is the built-in rule: only its off is stored.
	if entry.NameBranch != nil && *entry.NameBranch {
		entry.NameBranch = nil
	}
	set := entry.Status != "" || entry.Assign != nil || entry.Branch != "" || entry.NameBranch != nil || entry.Repository != ""
	return entry, set, nil
}

// checkTime checks a time section and returns it with each ticket's site
// as its connection names it: nil when it sets nothing. Refusing epics is
// the built-in rule: only its off is stored.
func checkTime(t TimeSettings) (*TimeSettings, error) {
	for _, held := range []**TimeTicket{&t.Meetings, &t.TimeOff} {
		if *held == nil {
			continue
		}
		ticket := new(**held)
		*held = ticket
		if ticket.Tracker != store.TrackerKindJira {
			return nil, invalidSetup("%s names a %s ticket, and time is logged on Jira only", sectionTime, truncateID(ticket.Tracker))
		}
		key, err := checkSite(ticket.Tracker, ticket.Site, sectionTime, map[siteKey]bool{})
		if err != nil {
			return nil, err
		}
		ticket.Site = key.site
		if !IsTicketKey(ticket.Tracker, ticket.Key) {
			return nil, invalidSetup("%s names %q of %s, which is not a ticket key (WEB-1)", sectionTime, truncateID(ticket.Key), key.site)
		}
		ticket.Key = strings.ToUpper(ticket.Key)
	}
	if t.NeverEpics != nil && *t.NeverEpics {
		t.NeverEpics = nil
	}
	if t.Meetings == nil && t.TimeOff == nil && t.NeverEpics == nil {
		return nil, nil
	}
	return &t, nil
}

// checkEstimates checks an estimates section: nil when it sets nothing.
// Counting by the boards' estimates and planning poker are the built-in
// rules: only their other choice is stored.
func checkEstimates(e Estimates) (*Estimates, error) {
	switch e.Count {
	case "", estimatesCountTickets:
	case "estimates":
		e.Count = ""
	default:
		return nil, invalidSetup("%s counts by %q, which is neither estimates nor tickets", sectionEstimates, truncateID(e.Count))
	}
	if e.Poker != nil && *e.Poker {
		e.Poker = nil
	}
	if e.Count == "" && e.Poker == nil {
		return nil, nil
	}
	return &e, nil
}

// checkCredit checks a credit section and returns it normalized: each
// snapshot's actions in creditActions' order, each support entry's site
// as its connection names it and its lists never null, the entries in
// site order without one that names nothing. Every snapshot names its
// From (stampCredit stamps a replace's newest one), the first 0 and each
// later one after the one before it.
func checkCredit(c Credit) (*Credit, error) {
	if len(c.Rules) == 0 {
		return nil, invalidSetup("%s has no rules", sectionCredit)
	}
	rules := make([]CreditRules, len(c.Rules))
	for i, r := range c.Rules {
		switch {
		case r.From == nil:
			return nil, invalidSetup("rule %d of %s says no time it took effect", i+1, sectionCredit)
		case i == 0 && *r.From != 0:
			return nil, invalidSetup("the first rule of %s takes effect at %d, not at the start (0)", sectionCredit, *r.From)
		case i > 0 && *r.From <= *rules[i-1].From:
			return nil, invalidSetup("rule %d of %s takes effect at %d, not after the rule before it (%d)", i+1, sectionCredit, *r.From, *rules[i-1].From)
		case r.ReviewShare < 0 || r.ReviewShare > creditMaxShare:
			return nil, invalidSetup("rule %d of %s gives a reviewer %d%% of a ticket's points; the share is 0 to %d", i+1, sectionCredit, r.ReviewShare,
				creditMaxShare)
		case r.HoursPerPoint < 1 || r.HoursPerPoint > creditMaxHoursPerPoint:
			return nil, invalidSetup("rule %d of %s counts a point for every %d hours of support; it is 1 to %d", i+1, sectionCredit, r.HoursPerPoint,
				creditMaxHoursPerPoint)
		}
		actions := make([]string, 0, len(creditActions))
		for _, a := range r.ReviewActions {
			if !slices.Contains(creditActions, a) {
				return nil, invalidSetup("rule %d of %s counts review action %q, which is none of %s", i+1, sectionCredit, truncateID(a),
					strings.Join(creditActions, ", "))
			}
			if slices.Contains(actions, a) {
				return nil, invalidSetup("rule %d of %s counts review action %s twice", i+1, sectionCredit, a)
			}
			actions = append(actions, a)
		}
		if len(actions) == 0 {
			return nil, invalidSetup("rule %d of %s counts no review action", i+1, sectionCredit)
		}
		slices.SortFunc(actions, func(a, b string) int { return slices.Index(creditActions, a) - slices.Index(creditActions, b) })
		if r.ReviewNeed < 1 || r.ReviewNeed > len(actions) {
			return nil, invalidSetup("rule %d of %s needs %d kinds of review action of the %d it counts", i+1, sectionCredit, r.ReviewNeed, len(actions))
		}
		from := *r.From
		r.From, r.ReviewActions = &from, actions
		rules[i] = r
	}
	out := &Credit{On: c.On, Rules: rules, Support: []SiteSupport{}}
	seen := map[siteKey]bool{}
	for _, entry := range c.Support {
		key, err := checkSite(entry.Tracker, entry.Site, sectionCredit, seen)
		if err != nil {
			return nil, err
		}
		if key.tracker != store.TrackerKindJira {
			return nil, invalidSetup("%s names support tickets of linear %s, and credit is counted on Jira only", sectionCredit, key.site)
		}
		entry.Site = key.site
		for _, list := range []struct {
			what string
			ids  *[]string
		}{{"support issue type", &entry.Types}, {"support label", &entry.Labels}, {"support board", &entry.Boards}} {
			if err := checkSetupList(key, "lists", list.what, list.ids, creditMaxSupport); err != nil {
				return nil, err
			}
			*list.ids = slices.Clone(slicesx.OrEmpty(*list.ids))
		}
		if len(entry.Types)+len(entry.Labels)+len(entry.Boards) > 0 {
			out.Support = append(out.Support, entry)
		}
	}
	slices.SortFunc(out.Support, func(a, b SiteSupport) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	return out, nil
}

// stored is whether c says more than no section does (defaultCredit),
// which is the one case encodeSetup writes a credit section.
func (c *Credit) stored() bool {
	return c != nil && (c.On || len(c.Support) > 0 || !reflect.DeepEqual(c.Rules, defaultCredit().Rules))
}

// isTicketProject is whether project is a ticket key's project, as
// TicketKeys spells it: a capital letter, then one or more capitals,
// digits or underscores.
func isTicketProject(project string) bool {
	if len(project) < 2 || len(project) > setupMaxID || project[0] < 'A' || project[0] > 'Z' {
		return false
	}
	for i := 1; i < len(project); i++ {
		c := project[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// checkSetupID checks a status, field or section ID.
func checkSetupID(what, id string) error {
	if id == "" || len(id) > setupMaxID {
		return invalidSetup("a %s ID is empty or over %d bytes: %q", what, setupMaxID, truncateID(id))
	}
	return nil
}

// checkSetupList checks a list of key's site (what an entry does with it,
// verb, "pins"): at most most IDs, each valid and named once.
func checkSetupList(key siteKey, verb, what string, list *[]string, most int) error {
	if list == nil {
		return nil
	}
	if len(*list) > most {
		return invalidSetup("%s %s %s %d %ss, over %d", key.tracker, key.site, verb, len(*list), what, most)
	}
	seen := make(map[string]bool, len(*list))
	for _, id := range *list {
		if err := checkSetupID(what, id); err != nil {
			return err
		}
		if seen[id] {
			return invalidSetup("%s %s %s %s %q twice", key.tracker, key.site, verb, what, id)
		}
		seen[id] = true
	}
	return nil
}

// truncateID is id cut to what a refusal quotes.
func truncateID(id string) string {
	if len(id) > 64 {
		return id[:64] + "…"
	}
	return id
}

func compareSites(trackerA, siteA, trackerB, siteB string) int {
	if c := strings.Compare(trackerA, trackerB); c != 0 {
		return c
	}
	return strings.Compare(siteA, siteB)
}
