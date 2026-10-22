package provider

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// SessionStartHook is the command a session runs at its start, after a
// compaction and after a clear, and at a resume when Resume is set, whose
// output reaches the model as context (the memory delivery,
// docs/specs/memory.md "Delivery"). Claude registers it in the session's
// inline settings and Codex in the thread's config; each provider reports a
// warning when a start, a compaction or a resume that ran it ends without
// its output (SessionStartDelivery).
type SessionStartHook struct {
	// Label names what the hook delivers in a warning, e.g. the memory
	// package's Label.
	Label string
	// Command is the shell command line the provider runs.
	Command string
	// Heading is the text the hook's context opens with, by which the
	// provider tells its output from the user's own SessionStart hooks'.
	Heading string
	// Ending is the text the hook's context ends with. claude-tui reads the
	// context from one block that joins it with the user's hooks' contexts,
	// and the ending is where it stops.
	Ending string
	// Stamp opens the line after Heading in each run's context, followed by
	// the run's time in RFC 3339. claude-tui reads the context out of a
	// model request that carries earlier runs' as well, and counts one only
	// when its time is not before the event it expects.
	Stamp string
	// Resume runs the hook at the resume the session starts with as well:
	// the thread's model does not hold the hook's current context
	// (SessionStartReceipts).
	Resume bool
}

// SessionStartHookSources are the SessionStart sources the hook runs for:
// each one leaves the model without the hook's earlier output, which a
// resumed history still carries (docs/specs/memory.md, "Delivery").
const SessionStartHookSources = "startup|compact|clear"

// SessionStartResumeSources are the sources of a hook that runs at a resume
// as well: Claude Code's `resume`, or `fork` for a resume that forks the
// session, and Codex's `resume`.
const SessionStartResumeSources = SessionStartHookSources + "|resume|fork"

// SessionStartMatcher is the matcher of the hook's group: the resume sources
// when it runs at a resume.
func SessionStartMatcher(resume bool) string {
	if resume {
		return SessionStartResumeSources
	}
	return SessionStartHookSources
}

// SessionStartHookTimeoutSeconds bounds one run of a SessionStartHook on
// both providers.
const SessionStartHookTimeoutSeconds = 60

// SessionStartEvent is what a SessionStartDelivery is expected after.
type SessionStartEvent string

const (
	SessionStartAfterStart      SessionStartEvent = "start"
	SessionStartAfterCompaction SessionStartEvent = "compaction"
	// SessionStartAfterResume expects the hook only when its Resume is set.
	SessionStartAfterResume SessionStartEvent = "resume"
)

// SessionStartReceipts keeps, across a thread's sessions, the context its
// model received from the SessionStart hook, so a resume runs the hook only
// when the model does not hold its context (SessionStartHook.Resume). A nil
// *SessionStartReceipts keeps nothing. A revert that cuts the history a
// delivery reached forgets the receipt outside the session.
type SessionStartReceipts struct {
	// Keep records that provider session sessionRef received context from
	// the hook.
	Keep func(sessionRef, context string) error
	// Forget records that the thread's model does not hold the hook's
	// context: a compaction replaced it.
	Forget func() error
}

// sessionStartFailureMaxBytes bounds the failure a warning quotes.
const sessionStartFailureMaxBytes = 300

// SessionStartDelivery follows whether a session's SessionStartHook
// delivered its context after a start, a compaction or a resume, and keeps
// the receipt of what it delivered. The provider calls Expect when the
// session starts, resumes or compacts, Deliver for each SessionStart output
// it sees, Fail for each failed run, Settle at the point by which the hook
// must have run, and Keep once the provider's own history holds what it
// delivered; Settle warns when the output never came and holds the receipt
// Keep writes. A nil *SessionStartDelivery, which a session without the hook
// holds, does nothing. Safe for concurrent use.
type SessionStartDelivery struct {
	receipts *SessionStartReceipts
	hook     SessionStartHook

	mu sync.Mutex
	// after is the event the open expectation follows, "" when none is
	// open.
	after SessionStartEvent
	// delivered is set once the hook's output was seen since the last
	// Settle, and context holds that output.
	delivered bool
	context   string
	failure   string
	// held is the receipt Settle holds for Keep.
	held heldReceipt
}

// heldReceipt is a receipt Settle holds: the context the hook delivered to
// the provider session sessionRef. A zero heldReceipt holds none.
type heldReceipt struct {
	sessionRef, context string
}

// NewSessionStartDelivery follows hook and keeps its receipts in receipts,
// which may be nil; nil when hook is. It refuses a hook without a label,
// command, heading, ending or stamp.
func NewSessionStartDelivery(hook *SessionStartHook, receipts *SessionStartReceipts) (*SessionStartDelivery, error) {
	if hook == nil {
		return nil, nil
	}
	if hook.Label == "" || hook.Command == "" || hook.Heading == "" || hook.Ending == "" || hook.Stamp == "" {
		return nil, errors.New("SessionStart hook: label, command, heading, ending and stamp are all required")
	}
	return &SessionStartDelivery{receipts: receipts, hook: *hook}, nil
}

// Hook is the hook the delivery follows; nil for a nil delivery.
func (d *SessionStartDelivery) Hook() *SessionStartHook {
	if d == nil {
		return nil
	}
	hook := d.hook
	return &hook
}

// Expect records that the hook must deliver before the next Settle after
// after: always after a start or a compaction, after a resume when its
// Resume is set. A delivery already seen since the last Settle counts:
// Claude Code reports a compaction's hook output before the compaction
// itself. A compaction forgets the receipt, kept or held, so the receipt
// names only what the compaction delivers; the warning it returns, "" when
// there is none, is for a failure to record that.
func (d *SessionStartDelivery) Expect(after SessionStartEvent) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	d.expect(after)
	d.mu.Unlock()
	return d.forget(after)
}

// ExpectAfresh is Expect after forgetting the delivery and failure seen
// since the last Settle, for a provider that runs a compaction's hook only
// after it reports the compaction: what it saw before came from an earlier
// start.
func (d *SessionStartDelivery) ExpectAfresh(after SessionStartEvent) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	d.delivered, d.context, d.failure = false, "", ""
	d.expect(after)
	d.mu.Unlock()
	return d.forget(after)
}

// expect opens the expectation after. Called with mu held.
func (d *SessionStartDelivery) expect(after SessionStartEvent) {
	if after == SessionStartAfterCompaction {
		d.held = heldReceipt{}
	}
	d.after = after
	if after == SessionStartAfterResume && !d.hook.Resume {
		d.after = ""
	}
}

// forget drops the receipt after a compaction.
func (d *SessionStartDelivery) forget(after SessionStartEvent) string {
	if after != SessionStartAfterCompaction || d.receipts == nil || d.receipts.Forget == nil {
		return ""
	}
	if err := d.receipts.Forget(); err != nil {
		return fmt.Sprintf("Agent Overflow could not record that this compaction replaced what the SessionStart hook delivered, so a later resume may not deliver %s again: %v", d.hook.Label, err)
	}
	return ""
}

// Expecting reports the event a delivery is expected after and not yet
// settled, "" when none is.
func (d *SessionStartDelivery) Expecting() SessionStartEvent {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.after
}

// Pending reports whether the open expectation still waits for the hook's
// output.
func (d *SessionStartDelivery) Pending() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.after != "" && !d.delivered
}

// Deliver records SessionStart context a provider reported whole when it
// opens with the hook's heading; another hook's context is ignored.
func (d *SessionStartDelivery) Deliver(context string) {
	if d == nil || !strings.HasPrefix(context, d.hook.Heading) {
		return
	}
	d.mu.Lock()
	d.delivered, d.context = true, context
	d.mu.Unlock()
}

// DropDeliveries forgets what the hook delivered and failed since the last
// Settle, and the receipt Settle holds, for a revert that cut them out of
// the history: the open expectation waits for the hook again.
func (d *SessionStartDelivery) DropDeliveries() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.delivered, d.context, d.failure = false, "", ""
	d.held = heldReceipt{}
	d.mu.Unlock()
}

// Fail records why a SessionStart run failed, as the provider reports it
// (an exit code, the run's error output). The first failure since the last
// Settle is the one a warning quotes.
func (d *SessionStartDelivery) Fail(detail string) {
	if d == nil {
		return
	}
	detail = sessionStartFailureText(detail)
	if detail == "" {
		return
	}
	d.mu.Lock()
	if d.failure == "" {
		d.failure = detail
	}
	d.mu.Unlock()
}

// Settle ends the expectation, holds a receipt under sessionRef when the
// hook's output arrived, and forgets what was delivered and failed. It
// returns the warning when the expected output did not arrive, "" when it
// did or none was expected. A session whose provider session is not known
// yet ("") holds no receipt, so its next resume delivers again.
func (d *SessionStartDelivery) Settle(sessionRef string) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	after, failure, delivered := d.after, d.failure, d.delivered
	if d.context != "" && sessionRef != "" {
		d.held = heldReceipt{sessionRef: sessionRef, context: d.context}
	}
	d.after, d.failure, d.delivered, d.context = "", "", false, ""
	d.mu.Unlock()
	if after == "" || delivered {
		return ""
	}
	if failure != "" {
		return fmt.Sprintf("%s did not reach this session after its %s: a SessionStart hook failed (%s).", d.hook.Label, after, failure)
	}
	return fmt.Sprintf("%s did not reach this session after its %s: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings.", d.hook.Label, after)
}

// Keep writes the receipt Settle holds, once the provider's history holds
// what it names, and returns the warning when it could not, "" otherwise.
func (d *SessionStartDelivery) Keep() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	held := d.held
	d.held = heldReceipt{}
	d.mu.Unlock()
	if held.context == "" || d.receipts == nil || d.receipts.Keep == nil {
		return ""
	}
	if err := d.receipts.Keep(held.sessionRef, held.context); err != nil {
		return fmt.Sprintf("Agent Overflow could not record that %s reached this session, so its next resume delivers it again: %v", d.hook.Label, err)
	}
	return ""
}

// sessionStartFailureText is detail on one line, without control or format
// characters (a run's error output can hold terminal escapes), and cut to
// sessionStartFailureMaxBytes.
func sessionStartFailureText(detail string) string {
	detail = strings.Join(strings.FieldsFunc(detail, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.In(r, unicode.Cc, unicode.Cf)
	}), " ")
	if len(detail) <= sessionStartFailureMaxBytes {
		return detail
	}
	cut := sessionStartFailureMaxBytes
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut] + "…"
}
