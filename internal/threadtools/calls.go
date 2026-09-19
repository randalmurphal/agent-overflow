package threadtools

// The write half of the App contract. Every call below has already been
// validated by this package: ids are resolved, strings are trimmed and
// non-empty, enums are canonical and bounds are inside their limits. An
// implementation still rechecks what only it can know (a provider this
// computer does not offer, a thread that vanished between resolution and
// the call) and returns a public error for it.

// SpawnCall creates a visible thread and runs the prompt there.
type SpawnCall struct {
	Prompt string `json:"prompt"`
	Title  string `json:"title,omitempty"`
	// ComputerID is empty for the caller's own computer.
	ComputerID string `json:"computer_id,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
	// WorkspacePath selects a checkout of that project. Empty inherits
	// the caller's workspace locally and is refused with ProjectID on
	// another computer unless WorktreeBranch is set.
	WorkspacePath string `json:"workspace_path,omitempty"`
	// WorktreeBranch is the branch of a fresh worktree cut through the
	// same draft-worktree path the sidebar uses. Empty means no worktree,
	// so there is no worktree without a branch name.
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	// FromThread forks that thread's history at its tail first. The fork
	// runs on the source thread's computer, in its project and workspace.
	FromThread         string `json:"from_thread,omitempty"`
	FromThreadComputer string `json:"from_thread_computer,omitempty"`
	// The five inherited settings. Empty means inherit the caller's.
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RuntimeMode string `json:"runtime_mode,omitempty"`
	// WaitSeconds is how long the call waits, not how long the work may
	// run. Notify delivers the answer as a message when the wait did not.
	WaitSeconds int  `json:"wait_seconds"`
	Notify      bool `json:"notify"`
}

// SendCall continues an existing thread as if the user had typed it.
type SendCall struct {
	ThreadID    string `json:"thread_id"`
	ComputerID  string `json:"computer_id,omitempty"`
	Message     string `json:"message"`
	WaitSeconds int    `json:"wait_seconds"`
	Notify      bool   `json:"notify"`
}

// AskCall forks the target into a hidden read-only scratch thread.
type AskCall struct {
	ThreadID    string `json:"thread_id"`
	ComputerID  string `json:"computer_id,omitempty"`
	Question    string `json:"question"`
	WaitSeconds int    `json:"wait_seconds"`
	Notify      bool   `json:"notify"`
}

// ReplyCall settles one request the caller's thread was asked.
type ReplyCall struct {
	Token string `json:"token"`
	Text  string `json:"text"`
}

// StatusCall re-attaches to requests or watches threads.
type StatusCall struct {
	Tokens    []string `json:"tokens,omitempty"`
	ThreadIDs []string `json:"thread_ids,omitempty"`
	// ThreadComputers pairs with ThreadIDs by index and holds the
	// computer each resolved to, empty for the caller's own.
	ThreadComputers []string `json:"thread_computers,omitempty"`
	WaitSeconds     int      `json:"wait_seconds"`
	// AfterRevision makes a wait skip what the caller has already seen.
	AfterRevision int64 `json:"after_revision,omitempty"`
}

// ListCall lists the caller's own requests.
type ListCall struct {
	// Offset continues a listing. It comes from this package's cursor.
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

// CancelCall cancels one request or interrupts one thread.
type CancelCall struct {
	Token      string `json:"token,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
}

// RemindCall arms a clock-settled request.
type RemindCall struct {
	// DueAtUnixMs is the absolute due time. This package resolves
	// after_seconds against the clock before calling.
	DueAtUnixMs int64  `json:"due_at"`
	Note        string `json:"note"`
}

// UpdateCall is one organize patch for threads on one computer.
type UpdateCall struct {
	ThreadIDs []string `json:"thread_ids"`
	// Each field is set only when the tool call carried it.
	Title    *string `json:"title,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
	Pin      *string `json:"pin,omitempty"`
	// Group is a group name in the thread's own project, created when it
	// does not exist. A set pointer to an empty string ungroups.
	Group *string `json:"group,omitempty"`
}

// Patched reports whether the patch changes anything.
func (c UpdateCall) Patched() bool {
	return c.Title != nil || c.Archived != nil || c.Pin != nil || c.Group != nil
}

// GroupCall renames, deletes or pins one group.
type GroupCall struct {
	GroupID   string `json:"group_id,omitempty"`
	Group     string `json:"group,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	// Exactly one of the three is set.
	Rename string `json:"rename,omitempty"`
	Pin    string `json:"pin,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// Request states, the source row's lifecycle.
const (
	RequestUnconfirmed = "unconfirmed"
	RequestAccepted    = "accepted"
	RequestRunning     = "running"
	RequestBlocked     = "blocked"
	RequestReplied     = "replied"
	RequestFinished    = "finished"
	RequestErrored     = "errored"
	RequestCancelled   = "cancelled"
	RequestInterrupted = "interrupted"
	RequestExpired     = "expired"
	RequestRefused     = "refused"
)

// Answer kinds. A final fallback is never mistaken for the answer the
// caller asked for because the kind is always stated.
const (
	AnswerReply = "reply"
	AnswerFinal = "final"
	AnswerError = "error"
	AnswerNote  = "note"
)

// Call outcomes. A settled call carries the answer; anything else carries
// the token and the work continues.
const (
	OutcomeSettled      = "settled"
	OutcomeBackgrounded = "backgrounded"
	OutcomeBlocked      = "blocked"
	OutcomeUnconfirmed  = "unconfirmed"
)

// RequestAck is what a spawn, send, ask or remind returns.
type RequestAck struct {
	Token      string `json:"token"`
	Kind       string `json:"kind,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state"`
	Outcome    string `json:"outcome"`
	AnswerKind string `json:"answer_kind,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Revision   int64  `json:"revision"`
	Notify     bool   `json:"notify"`
	// Delivered is inline, queued or draft once a wake has been handed
	// over, and empty while nothing has been delivered.
	Delivered string `json:"delivered,omitempty"`
	// ExpiresAt is when an uncollected answer is dropped on the computer
	// that holds it, in Unix milliseconds.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// ReplyAck is what thread_reply returns.
type ReplyAck struct {
	Token string `json:"token"`
	// Accepted is false when the reply repeated one already stored; the
	// state says which, and a retry with the same text is not an error.
	Accepted bool   `json:"accepted"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
	Late     bool   `json:"late,omitempty"`
	// Target names the thread the reply answered, so the responder can
	// see where it went.
	SourceThreadID string `json:"source_thread_id,omitempty"`
	SourceComputer string `json:"source_computer,omitempty"`
}

// StatusReport answers thread_status.
type StatusReport struct {
	Requests []RequestState `json:"requests,omitempty"`
	Threads  []ThreadState  `json:"threads,omitempty"`
	// WokeOn names the token or thread id that ended a wait, empty when
	// the wait ran out.
	WokeOn string `json:"woke_on,omitempty"`
	// TimedOut is true when the wait ended on its own clock.
	TimedOut bool `json:"timed_out,omitempty"`
}

// RequestState is one request's state in a status report.
type RequestState struct {
	Token      string `json:"token"`
	Kind       string `json:"kind"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state"`
	AnswerKind string `json:"answer_kind,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Revision   int64  `json:"revision"`
	Notify     bool   `json:"notify"`
	Delivered  string `json:"delivered,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	// DueAt is set for a reminder that has not fired.
	DueAt int64 `json:"due_at,omitempty"`
	// WakeQueued is true when a wake for this answer is already in the
	// caller's message queue, so the reply can say it is also arriving.
	WakeQueued bool `json:"wake_queued,omitempty"`
}

// ThreadState is one watched thread's live state.
type ThreadState struct {
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state"`
	// Resting is true when the thread is not running and not blocked on a
	// person.
	Resting bool `json:"resting"`
}

// RequestListing is what thread_status returns without tokens or ids.
type RequestListing struct {
	Requests []RequestState `json:"requests"`
	More     bool           `json:"-"`
}

// CancelReport is what thread_cancel returns.
type CancelReport struct {
	Token      string `json:"token,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	State      string `json:"state"`
	// Effect says what actually happened: queued_message_removed,
	// turn_interrupted, reminder_dropped or nothing_to_stop.
	Effect string `json:"effect"`
}

// UpdateReport is one computer's per-id result for thread_update.
type UpdateReport struct {
	Results []ThreadUpdateResult `json:"results"`
}

// ThreadUpdateResult is one thread's outcome. A refusal names the thread
// and the reason and leaves that thread untouched.
type ThreadUpdateResult struct {
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	Updated    bool   `json:"updated"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
}

// GroupReport is what thread_group returns.
type GroupReport struct {
	GroupID    string `json:"group_id,omitempty"`
	Group      string `json:"group"`
	ProjectID  string `json:"project_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	// Action is renamed, deleted or pinned.
	Action string `json:"action"`
	Pin    string `json:"pin,omitempty"`
	// Ungrouped counts the threads a delete took out of the group.
	Ungrouped int `json:"ungrouped,omitempty"`
}
