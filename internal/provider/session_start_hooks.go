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
// compaction and after a clear, whose output reaches the model as context
// (the memory delivery, docs/specs/memory.md "Delivery"), and PromptCommand,
// which it runs at each prompt. Claude registers them in the session's
// inline settings and Codex in the thread's config; each provider reports a
// warning when a start or a compaction ends without Command's output
// (SessionStartDelivery).
type SessionStartHook struct {
	// Label names what the hook delivers in a warning, e.g. the memory
	// package's Label.
	Label string
	// Command is the shell command line the provider runs at SessionStart.
	Command string
	// PromptCommand is the shell command line the provider runs at each
	// UserPromptSubmit: what changed since the session's record of what it
	// was shown, nothing when nothing did. "" for none.
	PromptCommand string
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
	// Failure opens each line the hook's command writes to stderr, by which
	// Claude tells a failed run of PromptCommand from the user's own
	// UserPromptSubmit hooks'.
	Failure string
}

// SessionStartHookSources are the SessionStart sources the hook runs for:
// each one leaves the model without the hook's earlier output, which a
// resumed history still carries (docs/specs/memory.md, "Delivery").
const SessionStartHookSources = "startup|compact|clear"

// SessionStartResumeSources are the sources of a hook that runs at a resume
// as well: Codex's `resume`, for a cold reload of a thread whose expected
// delivery the unload dropped, and Claude Code's `resume` and `fork`.
const SessionStartResumeSources = SessionStartHookSources + "|resume|fork"

// SessionStartMatcher is the matcher of the hook's group: the resume sources
// when it runs at a resume.
func SessionStartMatcher(resume bool) string {
	if resume {
		return SessionStartResumeSources
	}
	return SessionStartHookSources
}

// SessionStartHookTimeoutSeconds bounds one run of a SessionStartHook's
// commands on both providers.
const SessionStartHookTimeoutSeconds = 60

// SessionStartEvent is what a SessionStartDelivery is expected after.
type SessionStartEvent string

const (
	SessionStartAfterStart      SessionStartEvent = "start"
	SessionStartAfterCompaction SessionStartEvent = "compaction"
)

// sessionStartFailureMaxBytes bounds the failure a warning quotes.
const sessionStartFailureMaxBytes = 300

// SessionStartDelivery follows whether a session's SessionStartHook
// delivered its context after a start or a compaction. The provider calls
// Expect when the session starts or compacts, Deliver for each SessionStart
// output it sees, Fail for each failed run, and Settle at the point by which
// the hook must have run; Settle warns when the output never came, and runs
// forget, since the thread's record of what its model was shown no longer
// holds. A nil *SessionStartDelivery, which a session without the hook
// holds, does nothing. Safe for concurrent use.
type SessionStartDelivery struct {
	hook   SessionStartHook
	forget func() error

	mu sync.Mutex
	// after is the event the open expectation follows, "" when none is
	// open.
	after SessionStartEvent
	// delivered is set once the hook's output was seen since the last
	// Settle.
	delivered bool
	failure   string
}

// NewSessionStartDelivery follows hook; nil when hook is. forget, which may
// be nil, forgets the thread's records of what its model was shown: Settle
// runs it when an expected delivery did not arrive, and ForgetWhilePending
// when the provider asks. It refuses a hook without a label, command,
// heading, ending or stamp.
func NewSessionStartDelivery(hook *SessionStartHook, forget func() error) (*SessionStartDelivery, error) {
	if hook == nil {
		return nil, nil
	}
	if hook.Label == "" || hook.Command == "" || hook.Heading == "" || hook.Ending == "" || hook.Stamp == "" {
		return nil, errors.New("SessionStart hook: label, command, heading, ending and stamp are all required")
	}
	return &SessionStartDelivery{hook: *hook, forget: forget}, nil
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
// after. A delivery already seen since the last Settle counts: Claude Code
// reports a compaction's hook output before the compaction itself.
func (d *SessionStartDelivery) Expect(after SessionStartEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.after = after
	d.mu.Unlock()
}

// ExpectAfresh is Expect after forgetting the delivery and failure seen
// since the last Settle, for a provider that runs a compaction's hook only
// after it reports the compaction: what it saw before came from an earlier
// start.
func (d *SessionStartDelivery) ExpectAfresh(after SessionStartEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.delivered, d.failure = false, ""
	d.after = after
	d.mu.Unlock()
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
	d.delivered = true
	d.mu.Unlock()
}

// Drop ends the open expectation without a warning, for a revert that may
// have cut the hook's output out of the history: the caller forgets the
// thread's records, so the next prompt delivers the whole memory.
func (d *SessionStartDelivery) Drop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.after, d.delivered, d.failure = "", false, ""
	d.mu.Unlock()
}

// ForgetWhilePending runs forget when the open expectation still waits for
// the hook's output and the provider will not run the hook before a later
// turn, which a stop of the session or of the app can prevent: the record
// says the model holds what its start or compaction removed, so the next
// prompt must deliver the whole memory. The expectation stays open. It
// returns the warning when forget failed, "" otherwise.
func (d *SessionStartDelivery) ForgetWhilePending() string {
	if d == nil || d.forget == nil || !d.Pending() {
		return ""
	}
	if err := d.forget(); err != nil {
		return fmt.Sprintf("Agent Overflow could not record that %s has not reached this session since its %s, so a later prompt may deliver only what changed: %v", d.hook.Label, d.Expecting(), err)
	}
	return ""
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

// Settle ends the expectation and forgets what was delivered and failed. It
// returns the warning when the expected output did not arrive, "" when it
// did or none was expected, and then runs forget: the next prompt delivers
// the whole memory.
func (d *SessionStartDelivery) Settle() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	after, failure, delivered := d.after, d.failure, d.delivered
	d.after, d.failure, d.delivered = "", "", false
	d.mu.Unlock()
	if after == "" || delivered {
		return ""
	}
	warning := fmt.Sprintf("%s did not reach this session after its %s: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings.", d.hook.Label, after)
	if failure != "" {
		warning = fmt.Sprintf("%s did not reach this session after its %s: a SessionStart hook failed (%s).", d.hook.Label, after, failure)
	}
	if d.forget != nil {
		if err := d.forget(); err != nil {
			warning += fmt.Sprintf(" Agent Overflow could not record that, so the next prompt may deliver only what changed: %v", err)
		} else {
			warning += " The next prompt delivers it whole."
		}
	}
	return warning
}

// PromptHookFailure is the warning for a failed run of the hook's
// PromptCommand, which the provider reports with detail.
func PromptHookFailure(hook SessionStartHook, detail string) string {
	return fmt.Sprintf("Changes to %s may not have reached this turn: its UserPromptSubmit hook failed (%s).", hook.Label, sessionStartFailureText(detail))
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
