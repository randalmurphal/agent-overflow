package accountswitch

import (
	"context"
	"time"

	"agent-overflow/internal/provider"
)

// Config is the user-tier auto-switch settings.
type Config struct {
	Strategy         string // StrategySoonestReset or StrategyAccountOrder
	ThresholdPercent int
	AutoContinue     bool
	// Revision is the settings revision taken before the values were read,
	// so every later write carries a higher one (RevokeWaitsBefore).
	Revision uint64
}

// Account is one saved account of a provider.
type Account struct {
	ID     string
	Label  string
	InPool bool // Auto-switch is ticked on its card
	Usable bool // false when it needs a sign-in
}

// Pool is a provider's account listing.
type Pool struct {
	SelectedID string
	// Pinned is true when a person or an outside login made the selection.
	Pinned   bool
	Accounts []Account // card order
}

// ThreadModel is a thread with a turn in flight and its model.
type ThreadModel struct {
	ThreadID string
	Model    string
}

// Report is the payload of event provider:account_auto_switch: one per
// switch the coordinator makes or fails to make.
type Report struct {
	Provider      string  `json:"provider"`
	FromAccountID string  `json:"fromAccountId,omitempty"`
	ToAccountID   string  `json:"toAccountId,omitempty"`
	FromLabel     string  `json:"fromLabel,omitempty"`
	ToLabel       string  `json:"toLabel,omitempty"`
	Reason        Reason  `json:"reason,omitempty"`
	LimitName     string  `json:"limitName,omitempty"`
	UsedPercent   float64 `json:"usedPercent,omitempty"`
	ResetsAt      int64   `json:"resetsAt,omitempty"` // Unix seconds
	Model         string  `json:"model,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// Deps are the capabilities the coordinator acts through. Provider names are
// the usage family, `claude` or `codex`. Every field is required.
type Deps struct {
	Now func() time.Time
	// Settings returns the current settings. Cheap, no I/O.
	Settings func() Config
	// Pool returns the provider's listing. Cheap, no I/O. After Switch
	// returns without error it reports the new selection.
	Pool func(provider string) Pool
	// Reading returns the cached usage reading of one account; UpdatedAt is
	// Unix milliseconds. Cheap, no I/O.
	Reading func(provider, accountID string) (provider.RateLimitsSnapshot, bool)
	// RefreshReading reads one account's usage over the network and updates
	// the cache before returning. Never called on the send path.
	RefreshReading func(ctx context.Context, provider, accountID string) error
	// RequestReading asks for a reading of the selected account soon and
	// returns at once.
	RequestReading func(provider string)
	// InFlight lists the provider's threads with a turn in flight.
	InFlight func(provider string) []ThreadModel
	// Switch selects the account as an automatic selection and requests a
	// reading of it. needsLogin is true when the account needs a sign-in; it
	// is then skipped.
	Switch func(provider, accountID string) (needsLogin bool, err error)
	// ReachesRunningTurns is true when a running session follows a switch.
	ReachesRunningTurns func(provider string) bool
	// ContinueModel returns the model a continue of the thread would run on.
	// ok is false for a thread that cannot receive one: workflow-owned,
	// hidden, archived or deleted. Cheap, and callable under the thread lock.
	ContinueModel func(threadID string) (model string, ok bool)
	// Continue queues ContinueMessage to the thread. It calls admit under the
	// thread lock and returns nil without queuing when admit returns false.
	// It may dispatch the message, and so call BeforeSend, before returning.
	Continue func(ctx context.Context, threadID string, admit func() bool) error
	// Report publishes a switch or failed switch. It may run on the send
	// path under a thread's action lock, so it must not take one.
	Report func(Report)
	// ThreadNotice writes a timeline notice in one thread. Same lock rule as
	// Report.
	ThreadNotice func(threadID, text string)
}

// Option configures a Coordinator.
type Option func(*Coordinator)

// WithCheckInterval sets the period of the waiting-thread check.
func WithCheckInterval(d time.Duration) Option {
	return func(c *Coordinator) { c.interval = d }
}
