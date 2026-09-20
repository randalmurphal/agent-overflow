package threadtools

import (
	"context"
	"encoding/json"
	"time"

	"agent-overflow/internal/mcpargs"
)

// Server is the ao-thread-tools contract bound to one app. It holds no
// per-call state: the shape is recomputed for each tools/list and each
// call, so pairing or unpairing a computer changes what a running session
// sees without restarting anything.
type Server struct {
	app App
	// now is the clock thread_remind resolves after_seconds against.
	// Tests replace it; nothing else does.
	now func() time.Time
}

// New returns a server over app.
func New(app App) *Server { return &Server{app: app, now: time.Now} }

// Call runs one tool for one calling thread and returns the result value
// the transport marshals. Every error it returns is public: the code and
// prose reach the model, the cause stays in the host log.
func (s *Server) Call(ctx context.Context, caller Caller, name string, args json.RawMessage) (any, error) {
	return s.call(ctx, caller, name, args, false)
}

// CallForwarded runs one tool another computer forwarded to this one. It is
// a second entry point rather than a flag on the context so a peer endpoint
// cannot forget the fan-out guard: a forwarded call runs on this computer
// alone, reaching the tools with no paired computers of its own, so nothing
// it does fans out again. Two computers paired with each other would
// otherwise answer each other's fan-out until the bounds ran out.
func (s *Server) CallForwarded(ctx context.Context, caller Caller, name string, args json.RawMessage) (any, error) {
	return s.call(ctx, caller, name, args, true)
}

func (s *Server) call(ctx context.Context, caller Caller, name string, args json.RawMessage, forwarded bool) (any, error) {
	if s == nil || s.app == nil {
		return nil, publicf(CodeInvalidRequest, "Thread tools are not available in this session.")
	}
	if caller.ThreadID == "" {
		return nil, publicf(CodeInvalidRequest, "Thread tools could not tell which thread called them.")
	}
	var computers []Computer
	if forwarded {
		// The App reads this back for the one decision that turns on where
		// the call came from: an export is written under a name the asking
		// computer can fetch.
		ctx = WithForwarded(ctx)
	} else {
		var err error
		computers, err = s.app.PairedComputers(ctx)
		if err != nil {
			return nil, err
		}
	}
	// The calling thread travels on the context as well as in the session,
	// because a forwarded call names the thread it came from and the App
	// interface's Peer takes nothing but a context.
	ctx = WithCaller(ctx, caller)
	call := &session{app: s.app, caller: caller, computers: computers, now: s.now}
	switch name {
	case "thread_search":
		return call.search(ctx, args)
	case "thread_show":
		return call.show(ctx, args)
	case "thread_item":
		return call.item(ctx, args)
	case "thread_options":
		return call.options(ctx, args)
	case "thread_spawn":
		return call.spawn(ctx, args)
	case "thread_send":
		return call.send(ctx, args)
	case "thread_ask":
		return call.ask(ctx, args)
	case "thread_reply":
		return call.reply(ctx, args)
	case "thread_status":
		return call.status(ctx, args)
	case "thread_cancel":
		return call.cancel(ctx, args)
	case "thread_update":
		return call.update(ctx, args)
	case "thread_group":
		return call.group(ctx, args)
	case "thread_remind":
		return call.remind(ctx, args)
	default:
		return nil, invalidf("There is no tool named %q. The thread tools are: %s.", name, joinNames(ToolNames))
	}
}

// forwardedKey marks a call that arrived from another computer.
type forwardedKey struct{}

// WithForwarded marks ctx as carrying a call from another computer. It is
// what CallForwarded stamps, and an App test that exercises a forwarded read
// directly; the app itself never calls it, so an ordinary call cannot claim
// to be a peer's.
func WithForwarded(ctx context.Context) context.Context {
	return context.WithValue(ctx, forwardedKey{}, true)
}

// Forwarded reports whether the call an App method is serving arrived from
// another computer.
func Forwarded(ctx context.Context) bool {
	value, _ := ctx.Value(forwardedKey{}).(bool)
	return value
}

// callerKey carries the calling thread on a call's context.
type callerKey struct{}

// WithCaller stamps the calling thread on ctx. Server.Call does it for
// every tool; an app implementing Peer reads it back with CallerFrom so a
// forwarded call can name the thread and computer it came from.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

// CallerFrom reads back what WithCaller stamped. The second result is
// false outside a tool call.
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}

// session is one call's view of the world: who is calling and which
// computers are reachable from them.
type session struct {
	app       App
	caller    Caller
	computers []Computer
	now       func() time.Time
}

func (c *session) paired() bool { return len(c.computers) > 0 }

// self is the caller's own computer, as this computer names itself.
func (c *session) self() Computer {
	return Computer{ID: c.caller.ComputerID, Name: c.caller.ComputerName}
}

// computerByID finds a paired computer. The caller's own id resolves to
// the local computer with local true.
func (c *session) computerByID(id string) (computer Computer, local bool, ok bool) {
	if id == "" || id == "local" || (c.caller.ComputerID != "" && id == c.caller.ComputerID) {
		return c.self(), true, true
	}
	for _, candidate := range c.computers {
		if candidate.ID == id {
			return candidate, false, true
		}
	}
	return Computer{}, false, false
}

// stamp writes the owning computer onto a row, and only in the paired
// shape: with no paired computer a result carries no computer field at
// all.
func (c *session) stamp(computer Computer) (id, name string) {
	if !c.paired() {
		return "", ""
	}
	if computer.ID == "" {
		computer = c.self()
	}
	if computer.Name == "" {
		// A row that carries an id alone still names the computer: a
		// request receipt records where the work went, not what this
		// computer calls that machine, and the name is what the model
		// reads back.
		if known, _, ok := c.computerByID(computer.ID); ok {
			computer.Name = known.Name
		}
	}
	return computer.ID, computer.Name
}

// decode parses a tool's arguments under the closed schema the transport
// uses, so an unknown field is refused here exactly as it is for the other
// built-in servers.
func decode(args json.RawMessage, target any) error {
	if err := mcpargs.Decode(args, target); err != nil {
		return publicf(CodeInvalidRequest, "%s", err.Error())
	}
	return nil
}

func joinNames(names []string) string {
	out := ""
	for index, name := range names {
		switch {
		case index == 0:
			out = name
		case index == len(names)-1:
			out += " and " + name
		default:
			out += ", " + name
		}
	}
	return out
}

// errorRow is how an unreachable or too-old computer appears in a grouped
// result: the call still succeeds and the other computers' rows return.
type errorRow struct {
	ComputerID string `json:"computer_id"`
	Computer   string `json:"computer"`
	Error      string `json:"error"`
	Code       string `json:"error_code,omitempty"`
}

func newErrorRow(computer Computer, err error) errorRow {
	code, message := publicMessage(err)
	return errorRow{ComputerID: computer.ID, Computer: computer.Name, Error: message, Code: code}
}

// peerResult decodes a result a peer produced with this same code. The
// destination answers in its own shape, so the caller re-stamps the rows
// it forwards rather than trusting the peer to know its own name here.
func peerResult(raw json.RawMessage, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return publicf(CodeUnreachable, "The other computer answered with a result this version cannot read: %v", err)
	}
	return nil
}
