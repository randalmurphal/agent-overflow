package provider

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// SessionStartHook is a command a session runs at its start, after a
// compaction and after a clear, and at a resume when Resume is set, whose
// output reaches the model as context (the memory delivery,
// docs/specs/memory.md "Delivery"). Claude registers it
// in the session's inline settings and Codex in the thread's config, each
// hook in a matcher group of its own; each provider reports a warning when a
// start, a compaction or a resume that ran it ends without its output
// (SessionStartDelivery).
type SessionStartHook struct {
	// Label names what the hook delivers in a warning and in its receipt,
	// e.g. the memory package's Label.
	Label string
	// Command is the shell command line the provider runs.
	Command string
	// Heading is the text the hook's context opens with, by which the
	// provider tells its output from another SessionStart hook's.
	Heading string
	// Ending is the text the hook's context ends with. claude-tui reads
	// every hook's context from one block that joins them, and the ending
	// is where this hook's stops.
	Ending string
	// Stamp opens the line after Heading in each run's context, followed by
	// the run's time in RFC 3339. claude-tui reads the contexts out of a
	// model request that carries earlier runs' as well, and counts one only
	// when its time is not before the event it expects.
	Stamp string
	// Resume runs the hook at the resume the session starts with as well:
	// the thread's model does not hold the hook's current context
	// (SessionStartReceipts).
	Resume bool
}

// SessionStartHookSources are the SessionStart sources every hook runs for:
// each one leaves the model without the hook's earlier output, which a
// resumed history still carries (docs/specs/memory.md, "Delivery").
const SessionStartHookSources = "startup|compact|clear"

// SessionStartResumeSources are the sources of a hook that runs at a resume
// as well: Claude Code's `resume`, or `fork` for a resume that forks the
// session, and Codex's `resume`.
const SessionStartResumeSources = SessionStartHookSources + "|resume|fork"

// SessionStartMatcher is the matcher of a hook's group: the resume sources
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
	// SessionStartAfterResume expects the hooks a resume runs, those whose
	// Resume is set.
	SessionStartAfterResume SessionStartEvent = "resume"
)

// SessionStartReceipts keeps, across a thread's sessions, the context its
// model received from each SessionStart hook, so a resume runs only the
// hooks whose context the model does not hold (SessionStartHook.Resume). A
// nil *SessionStartReceipts keeps nothing. A revert that cuts the history a
// delivery reached forgets the receipts outside the session.
type SessionStartReceipts struct {
	// Keep records that provider session sessionRef received context from
	// the hook labelled label.
	Keep func(sessionRef, label, context string) error
	// Forget records that the thread's model holds no hook's context: a
	// compaction replaced it.
	Forget func() error
}

// sessionStartFailureMaxBytes bounds the failure a warning quotes.
const sessionStartFailureMaxBytes = 300

// SessionStartDelivery follows whether a session's SessionStartHooks
// delivered their context after a start, a compaction or a resume, and keeps
// the receipt of what each delivered. The provider calls Expect when the
// session starts, resumes or compacts, Deliver for each SessionStart output
// it sees, Fail for each failed run, Settle at the point by which the hooks
// must have run, and Keep once the provider's own history holds what they
// delivered; Settle names the hooks whose output never came and holds the
// receipts Keep writes. A nil *SessionStartDelivery, which a session without
// hooks holds, does nothing. Safe for concurrent use.
type SessionStartDelivery struct {
	receipts *SessionStartReceipts

	mu    sync.Mutex
	hooks []SessionStartHook
	after SessionStartEvent
	// expected marks the hooks the open expectation waits for.
	expected []bool
	// delivered marks the hooks whose output was seen since the last
	// Settle, and contexts holds that output.
	delivered []bool
	contexts  []string
	failure   string
	// held are the receipts Settle holds for Keep, by hook.
	held []heldReceipt
}

// heldReceipt is a receipt Settle holds: the context a hook delivered to the
// provider session sessionRef. A zero heldReceipt holds none.
type heldReceipt struct {
	sessionRef, context string
}

// NewSessionStartDelivery follows hooks and keeps their receipts in
// receipts, which may be nil; nil when there are no hooks. It refuses a hook
// without a label, command, heading, ending or stamp, and two hooks with the
// same label or heading: a heading is how a delivery is told apart, and a
// label is what its receipt is kept under.
func NewSessionStartDelivery(hooks []SessionStartHook, receipts *SessionStartReceipts) (*SessionStartDelivery, error) {
	if len(hooks) == 0 {
		return nil, nil
	}
	labels := make(map[string]bool, len(hooks))
	headings := make(map[string]bool, len(hooks))
	for i, hook := range hooks {
		if hook.Label == "" || hook.Command == "" || hook.Heading == "" || hook.Ending == "" || hook.Stamp == "" {
			return nil, fmt.Errorf("SessionStart hook %d: label, command, heading, ending and stamp are all required", i)
		}
		if labels[hook.Label] {
			return nil, fmt.Errorf("SessionStart hook %d: label %q is another hook's", i, hook.Label)
		}
		if headings[hook.Heading] {
			return nil, fmt.Errorf("SessionStart hook %s: heading %q is another hook's", hook.Label, hook.Heading)
		}
		labels[hook.Label] = true
		headings[hook.Heading] = true
	}
	return &SessionStartDelivery{
		receipts:  receipts,
		hooks:     hooks,
		expected:  make([]bool, len(hooks)),
		delivered: make([]bool, len(hooks)),
		contexts:  make([]string, len(hooks)),
		held:      make([]heldReceipt, len(hooks)),
	}, nil
}

// Expect records that the hooks after runs must deliver before the next
// Settle: every hook after a start or a compaction, those whose Resume is
// set after a resume. Deliveries already seen since the last Settle count:
// Claude Code reports a compaction's hook output before the compaction
// itself. A compaction forgets every receipt, kept or held, so the receipts
// name only what the compaction delivers; the warning it returns, "" when
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

// ExpectAfresh is Expect after forgetting the deliveries and failures seen
// since the last Settle, for a provider that runs a compaction's hooks only
// after it reports the compaction: what it saw before came from an earlier
// start.
func (d *SessionStartDelivery) ExpectAfresh(after SessionStartEvent) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	clear(d.delivered)
	clear(d.contexts)
	d.failure = ""
	d.expect(after)
	d.mu.Unlock()
	return d.forget(after)
}

// expect opens the expectation after. Called with mu held.
func (d *SessionStartDelivery) expect(after SessionStartEvent) {
	if after == SessionStartAfterCompaction {
		clear(d.held)
	}
	d.after = ""
	for i, hook := range d.hooks {
		d.expected[i] = after != SessionStartAfterResume || hook.Resume
		if d.expected[i] {
			d.after = after
		}
	}
}

// forget drops the receipts after a compaction.
func (d *SessionStartDelivery) forget(after SessionStartEvent) string {
	if after != SessionStartAfterCompaction || d.receipts == nil || d.receipts.Forget == nil {
		return ""
	}
	if err := d.receipts.Forget(); err != nil {
		return fmt.Sprintf("Agent Overflow could not record that this compaction replaced what the SessionStart hooks delivered, so a later resume may not deliver %s again: %v", d.labels(), err)
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

// Pending reports, per hook, whether the open expectation still waits for
// it; nil when none is open.
func (d *SessionStartDelivery) Pending() []bool {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.after == "" {
		return nil
	}
	pending := make([]bool, len(d.hooks))
	for i := range d.hooks {
		pending[i] = d.expected[i] && !d.delivered[i]
	}
	return pending
}

// Deliver records SessionStart context a provider reported whole: the
// output of the hook whose heading it opens with, if any.
func (d *SessionStartDelivery) Deliver(context string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, hook := range d.hooks {
		if strings.HasPrefix(context, hook.Heading) {
			d.delivered[i] = true
			d.contexts[i] = context
			return
		}
	}
}

// DropDeliveries forgets what the hooks delivered and failed since the last
// Settle, and the receipts Settle holds, for a revert that cut them out of
// the history: the open expectation waits for the hooks again.
func (d *SessionStartDelivery) DropDeliveries() {
	if d == nil {
		return
	}
	d.mu.Lock()
	clear(d.delivered)
	clear(d.contexts)
	clear(d.held)
	d.failure = ""
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

// Settle ends the expectation, holds a receipt under sessionRef for each
// hook whose output arrived, and forgets what was delivered and failed. It
// returns the warning for the hooks whose output did not arrive, if any. A
// session whose provider session is not known yet ("") holds no receipt, so
// its next resume delivers again.
func (d *SessionStartDelivery) Settle(sessionRef string) []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	after, failure := d.after, d.failure
	d.after, d.failure = "", ""
	var missing []string
	for i, hook := range d.hooks {
		if after != "" && d.expected[i] && !d.delivered[i] {
			missing = append(missing, hook.Label)
		}
		if d.contexts[i] != "" && sessionRef != "" {
			d.held[i] = heldReceipt{sessionRef: sessionRef, context: d.contexts[i]}
		}
		d.expected[i], d.delivered[i], d.contexts[i] = false, false, ""
	}
	d.mu.Unlock()
	if len(missing) == 0 {
		return nil
	}
	return []string{missingWarning(missing, after, failure)}
}

// Keep writes the receipts Settle holds, once the provider's history holds
// what they name, and returns the warning for those it could not write.
func (d *SessionStartDelivery) Keep() []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	type receipt struct{ label, sessionRef, context string }
	var receipts []receipt
	for i, held := range d.held {
		if held.context != "" {
			receipts = append(receipts, receipt{d.hooks[i].Label, held.sessionRef, held.context})
		}
	}
	clear(d.held)
	d.mu.Unlock()
	if d.receipts == nil || d.receipts.Keep == nil {
		return nil
	}
	var unkept []string
	var errs []error
	for _, r := range receipts {
		if err := d.receipts.Keep(r.sessionRef, r.label, r.context); err != nil {
			unkept = append(unkept, r.label)
			errs = append(errs, err)
		}
	}
	if len(unkept) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Agent Overflow could not record that %s reached this session, so its next resume delivers %s again: %v", joinLabels(unkept), pronoun(unkept), errors.Join(errs...))}
}

// missingWarning names the hooks whose output did not arrive after an event,
// with the failure that explains it when one was reported.
func missingWarning(missing []string, after SessionStartEvent, failure string) string {
	subject := joinLabels(missing)
	if failure != "" {
		return fmt.Sprintf("%s did not reach this session after its %s: a SessionStart hook failed (%s).", subject, after, failure)
	}
	return fmt.Sprintf("%s did not reach this session after its %s: no SessionStart hook output carried %s. A hook failed, or hooks are turned off in the provider's own settings.", subject, after, pronoun(missing))
}

// labels names every hook.
func (d *SessionStartDelivery) labels() string {
	labels := make([]string, len(d.hooks))
	for i, hook := range d.hooks {
		labels[i] = hook.Label
	}
	return joinLabels(labels)
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

func joinLabels(labels []string) string {
	switch len(labels) {
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " and " + labels[1]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

func pronoun(labels []string) string {
	if len(labels) > 1 {
		return "them"
	}
	return "it"
}
