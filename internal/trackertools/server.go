// Package trackertools is the ao-tracker-tools contract: the tools an agent
// in a thread uses to find, read, create and change tickets in the user's
// issue tracker, their schemas, argument validation, how a ticket key
// names an issue on this computer's connections, what a write checks
// before it sends anything, the shape of each answer, and the guide the
// model reads. The product contract is
// docs/architecture/tracker-backend.md#agent-tools.
//
// It depends on the Tracker interface below, which the app implements over
// trackerapp with each read and write bounded as the matching bound method
// is, and on Host, the app's answers about the calling thread (whether it
// may write, the person's approval, its timeline row, a create's board);
// never on internal/app or the store. Which sessions have the server, and
// whether a capability URL still acts for its session, are the app's. The
// read marks (marks.go) are the server's own state, per session.
package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/mcpargs"
	"agent-overflow/internal/trackerapp"
)

// MCPServerName is the server's name in every provider's MCP configuration.
const MCPServerName = "ao-tracker-tools"

// Error codes. Every refusal the model can act on has a stable code; raw
// causes stay in host logs. A refusal trackerapp or the app already coded
// (a connection turned off, a tracker that failed) passes through with its
// own code.
const (
	// CodeInvalidRequest is an argument refused before anything was sent.
	CodeInvalidRequest = "tracker_request_invalid"
	// CodeNoConnection is a computer with no Jira or Linear connection on.
	CodeNoConnection = "tracker_no_connection"
	// CodeNoTicket is a read with no key from a thread linked to no ticket.
	CodeNoTicket = "tracker_no_ticket"
	// CodeNotFound is a key no connection on this computer can read.
	CodeNotFound = "tracker_not_found"
	// CodeAmbiguous is a key without a site that tickets on several
	// connections have.
	CodeAmbiguous = "tracker_ambiguous"
	// CodeFailed is a call that failed for a reason the model cannot act on;
	// the app logs the cause.
	CodeFailed = "tracker_call_failed"
	// CodeUnknown is a create whose answer never came: it may have been
	// made, and the same call again finds it.
	CodeUnknown = "tracker_outcome_unknown"
)

// Tracker is what the tools read and write through: this computer's
// ticket sources, a thread's linked ticket, a key's tickets and a board as
// the mirrors hold them, an agent's earlier writes (trackerapp), and the
// tracker reads and writes, each bounded by the app. connection is a
// TicketSource's ID or a LinkedTicket's Connection; issue is the tracker's
// issue ID. A write carries the agent that makes it (trackerapp.ByAgent).
type Tracker interface {
	TicketSources() ([]trackerapp.TicketSource, error)
	LinkedTicket(threadID string) (trackerapp.LinkedTicket, bool, error)
	TicketsOfKey(key string) ([]trackerapp.KeyedTicket, error)
	BoardSummary(connection, board string) (trackerapp.BoardSummary, error)
	MirrorCard(connection, issue string) (trackerapp.CardView, bool, error)
	AgentRepeat(threadID, digest string) (trackerapp.AgentRepeat, bool, error)
	SettleAgentRepeat(row int64, issue, comment string) error

	Lookup(ctx context.Context, connection string, keys []string) (trackerapp.LookupView, error)
	Search(ctx context.Context, connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error)
	IssueDetail(ctx context.Context, connection, issue string) (trackerapp.DetailView, error)
	Comments(ctx context.Context, connection, issue, page string) (trackerapp.CommentPageView, error)
	IssueTypes(ctx context.Context, connection, project string) ([]trackerapp.IssueTypeView, error)
	CreateFields(ctx context.Context, connection, project, issueType string) ([]trackerapp.FieldView, error)

	Create(ctx context.Context, connection string, in trackerapp.CreateRequest) (trackerapp.WriteView, error)
	Edit(ctx context.Context, connection string, in trackerapp.EditRequest) (trackerapp.WriteView, error)
	Move(ctx context.Context, connection string, in trackerapp.MoveRequest) (trackerapp.WriteView, error)
	SetEstimate(ctx context.Context, connection string, in trackerapp.EstimateRequest) (trackerapp.WriteView, error)
	MoveToSprint(ctx context.Context, connection string, in trackerapp.SprintMoveRequest) (trackerapp.WriteView, error)
	AddComment(ctx context.Context, connection string, in trackerapp.CommentRequest) (trackerapp.WriteView, error)
}

// Host is what the writing tools ask the app about the calling thread.
type Host interface {
	// Writable refuses, with a public error, a thread that may not change
	// tickets: a read-only thread.
	Writable(threadID string) error
	// Approve asks the person to approve a change and returns when they
	// answer: nil when they approve, a public error when they deny or do
	// not answer in time, and ctx's error when the call ends first.
	Approve(ctx context.Context, threadID string, a Approval) error
	// Wrote puts the row of a call that changed or created tickets in the
	// thread's timeline.
	Wrote(threadID string, row WriteRow)
	// CreateBoard is the board a create in project (its key) on connection
	// goes through: one of the thread's project's boards of that project,
	// else one any project lists; ok is false for none.
	CreateBoard(threadID, connection, project string) (board string, ok bool, err error)
}

// Approval is a change the person approves before it is sent: the call's
// ID, each ticket with the changes it gets, and the comment each gets.
type Approval struct {
	ID      string           `json:"id"`
	Tickets []ApprovalTicket `json:"tickets"`
	Comment string           `json:"comment,omitempty"`
}

// ApprovalTicket is one ticket of an Approval: its key as answers write
// it, its title, and each change as a person reads it ("Status → Done").
type ApprovalTicket struct {
	Key     string   `json:"key"`
	Title   string   `json:"title,omitempty"`
	Changes []string `json:"changes,omitempty"`
}

// WriteRow is what one call changed, for the thread's timeline: the
// call's ID, which names its row, and each ticket it created or changed,
// with what was applied.
type WriteRow struct {
	ID      string          `json:"-"`
	Created bool            `json:"created,omitempty"`
	Tickets []WrittenTicket `json:"tickets"`
}

// WrittenTicket is one ticket of a WriteRow, by the connection and issue
// that open it, its key as answers write it, its title, and the steps
// applied, by the names the call gave.
type WrittenTicket struct {
	Connection string   `json:"connection"`
	Issue      string   `json:"issue"`
	Key        string   `json:"key"`
	Title      string   `json:"title,omitempty"`
	Applied    []string `json:"applied"`
}

// Caller is who calls a tool: the thread, and the provider session its
// capability was issued to, which the read marks are kept for.
type Caller struct {
	Thread  string
	Session string
}

// Server is the ao-tracker-tools contract bound to one Tracker and Host.
type Server struct {
	t     Tracker
	h     Host
	marks marks
	// newID mints a call's ID, which names its approval and its records.
	newID func() string
	// logf records what fails after the call's outcome is decided.
	logf func(format string, args ...any)
}

// New returns a server over t and h.
func New(t Tracker, h Host) *Server {
	return &Server{t: t, h: h, newID: func() string { return uuid.NewString() }, logf: log.Printf}
}

// EndSession forgets the read marks of session, whose capability ended.
func (s *Server) EndSession(session string) { s.marks.end(session) }

// Call runs one tool for caller and returns the value the transport
// marshals. Every error it returns that the model can act on is public
// (errorsx.Public or errorsx.Detailed); any other is a failure the caller
// logs and answers generically.
func (s *Server) Call(ctx context.Context, caller Caller, name string, args json.RawMessage) (any, error) {
	if caller.Thread == "" || caller.Session == "" {
		return nil, invalidf("The tracker tools could not tell which thread called them.")
	}
	for _, spec := range toolTable {
		if spec.name == name {
			return spec.call(s, ctx, caller, args)
		}
	}
	return nil, invalidf("There is no tool named %q. The tracker tools are: %s.", name, strings.Join(ToolNames(), ", "))
}

func invalidf(format string, args ...any) error {
	return errorsx.Public(CodeInvalidRequest, fmt.Sprintf(format, args...), nil)
}

// decode parses arguments under the closed schema; a refusal names the field.
func decode(raw json.RawMessage, target any) error {
	if err := mcpargs.Decode(raw, target); err != nil {
		return errorsx.Public(CodeInvalidRequest, err.Error(), nil)
	}
	return nil
}

// sources is the connections a key or a search runs on, in the order they
// were made; none is a refusal that says how to get one.
func (s *Server) sources() ([]trackerapp.TicketSource, error) {
	sources, err := s.t.TicketSources()
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, errorsx.Public(CodeNoConnection,
			"This computer has no Jira or Linear connection turned on. The user adds or turns one on in the Issues settings.", nil)
	}
	return sources, nil
}

// onSite is err as a call on site fails: a public refusal's sentence names
// the site, so an answer drawn from several connections says which failed.
func onSite(site string, err error) error {
	code, message, ok := errorsx.PublicDetails(err)
	if !ok {
		return fmt.Errorf("%s: %w", site, err)
	}
	return errorsx.Public(code, site+": "+message, err)
}

// siteList is sources' sites as a sentence names them, each once though
// two connections reach it: "a", "a or b", "a, b, or c".
func siteList(sources []trackerapp.TicketSource) string {
	var names []string
	for _, src := range sources {
		if !slices.Contains(names, src.Site) {
			names = append(names, src.Site)
		}
	}
	return sayList(names, "or")
}

// sayList is names joined as a sentence does with conjunction and: "a",
// "a and b", "a, b, and c".
func sayList(names []string, and string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " " + and + " " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", " + and + " " + names[len(names)-1]
}

// notFound reports whether err is the tracker's answer that it holds no
// such issue (deleted, or not visible to the connection's account).
func notFound(err error) bool { return errors.Is(err, trackerapp.ErrIssueNotFound) }
