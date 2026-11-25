// Package trackertools is the ao-tracker-tools contract: the tools an agent
// in a thread uses to find and read tickets in the user's issue tracker, their
// schemas, argument validation, how a ticket key names an issue on this
// computer's connections, the shape of each answer, and the guide the model
// reads. The product contract is docs/architecture/tracker-backend.md#agent-tools.
//
// It depends on the Tracker interface below, which the app implements over
// trackerapp with each read bounded as the matching bound method is, and
// never on internal/app or the store. Which sessions have the server, and
// whether a capability URL still acts for its session, are the app's.
package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
)

// Tracker is what the tools read through: this computer's ticket sources
// and a thread's linked ticket (trackerapp), and the tracker reads, each
// bounded by the app. connection is a TicketSource's ID or a
// LinkedTicket's Connection; issue is the tracker's issue ID.
type Tracker interface {
	TicketSources() ([]trackerapp.TicketSource, error)
	LinkedTicket(threadID string) (trackerapp.LinkedTicket, bool, error)
	Lookup(ctx context.Context, connection string, keys []string) (trackerapp.LookupView, error)
	Search(ctx context.Context, connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error)
	IssueDetail(ctx context.Context, connection, issue string) (trackerapp.DetailView, error)
	Comments(ctx context.Context, connection, issue, page string) (trackerapp.CommentPageView, error)
}

// Server is the ao-tracker-tools contract bound to one Tracker.
type Server struct{ t Tracker }

// New returns a server over t.
func New(t Tracker) *Server { return &Server{t: t} }

// Call runs one tool for thread threadID and returns the value the
// transport marshals. Every error it returns that the model can act on is
// public (errorsx.Public or errorsx.Detailed); any other is a failure the
// caller logs and answers generically.
func (s *Server) Call(ctx context.Context, threadID, name string, args json.RawMessage) (any, error) {
	if threadID == "" {
		return nil, invalidf("The tracker tools could not tell which thread called them.")
	}
	for _, spec := range toolTable {
		if spec.name == name {
			return spec.call(s, ctx, threadID, args)
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

// siteList is sources' sites as a sentence names them: "a", "a or b",
// "a, b, or c".
func siteList(sources []trackerapp.TicketSource) string {
	names := make([]string, len(sources))
	for i, src := range sources {
		names[i] = src.Site
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
