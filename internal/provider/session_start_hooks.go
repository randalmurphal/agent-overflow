package provider

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// SessionStartHook is a command a session runs at its start, after a
// compaction and after a clear, whose output reaches the model as context
// (a standing agent's home files, docs/specs/standing-agents.md "Agent home
// contents"). Claude registers it in the session's inline settings and Codex
// in the thread's config; each provider reports a warning when a start or a
// compaction ends without its output (SessionStartDelivery).
type SessionStartHook struct {
	// Label names what the hook delivers in a warning, e.g. "instructions.md".
	Label string
	// Command is the shell command line the provider runs.
	Command string
	// Heading is the text the hook's context opens with, by which the
	// provider tells its output from another SessionStart hook's.
	Heading string
}

// SessionStartHookSources are the SessionStart sources the hooks run for:
// each one leaves the model without the hook's earlier output, which a
// resumed history still carries (docs/specs/standing-agents.md, "Agent home
// contents").
const SessionStartHookSources = "startup|compact|clear"

// SessionStartHookTimeoutSeconds bounds one run of a SessionStartHook on
// both providers.
const SessionStartHookTimeoutSeconds = 60

// SessionStartEvent is what a SessionStartDelivery is expected after.
type SessionStartEvent string

const (
	SessionStartAfterStart      SessionStartEvent = "start"
	SessionStartAfterCompaction SessionStartEvent = "compaction"
)

// sessionStartFailureMaxBytes bounds the failure a warning quotes.
const sessionStartFailureMaxBytes = 300

// SessionStartDelivery follows whether a session's SessionStartHooks
// delivered their context after a start or a compaction. The provider calls
// Expect when the session starts fresh or compacts, Deliver for each
// SessionStart output it sees, Fail for each failed run, and Settle at the
// point by which the hooks must have run; Settle names the hooks whose
// output never came. A nil *SessionStartDelivery, which a session without
// hooks holds, does nothing. Safe for concurrent use.
type SessionStartDelivery struct {
	mu        sync.Mutex
	hooks     []SessionStartHook
	after     SessionStartEvent
	delivered []bool
	failure   string
}

// NewSessionStartDelivery follows hooks; nil when there are none. It
// refuses a hook without a label, command or heading, and two hooks with
// the same heading: a heading is how a delivery is told apart.
func NewSessionStartDelivery(hooks []SessionStartHook) (*SessionStartDelivery, error) {
	if len(hooks) == 0 {
		return nil, nil
	}
	headings := make(map[string]bool, len(hooks))
	for i, hook := range hooks {
		if hook.Label == "" || hook.Command == "" || hook.Heading == "" {
			return nil, fmt.Errorf("SessionStart hook %d: label, command and heading are all required", i)
		}
		if headings[hook.Heading] {
			return nil, fmt.Errorf("SessionStart hook %s: heading %q is another hook's", hook.Label, hook.Heading)
		}
		headings[hook.Heading] = true
	}
	return &SessionStartDelivery{hooks: hooks, delivered: make([]bool, len(hooks))}, nil
}

// Expect records that the hooks must deliver before the next Settle.
// Deliveries already seen since the last Settle count: Claude Code reports
// a compaction's hook output before the compaction itself.
func (d *SessionStartDelivery) Expect(after SessionStartEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.after = after
	d.mu.Unlock()
}

// ExpectAfresh is Expect after forgetting the deliveries and failures seen
// since the last Settle, for a provider that runs a compaction's hooks only
// after it reports the compaction: what it saw before came from an earlier
// start.
func (d *SessionStartDelivery) ExpectAfresh(after SessionStartEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.after = after
	clear(d.delivered)
	d.failure = ""
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

// Deliver records SessionStart context a provider reported: the output of
// the hook whose heading it opens with, if any.
func (d *SessionStartDelivery) Deliver(context string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, hook := range d.hooks {
		if strings.HasPrefix(context, hook.Heading) {
			d.delivered[i] = true
			return
		}
	}
}

// DeliverWithin records every hook whose heading text holds: hook context
// a model request carries among other text.
func (d *SessionStartDelivery) DeliverWithin(text []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, hook := range d.hooks {
		if bytes.Contains(text, []byte(hook.Heading)) {
			d.delivered[i] = true
		}
	}
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
// returns the warning for the hooks whose output did not arrive, "" when
// all did or nothing was expected.
func (d *SessionStartDelivery) Settle() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	after, failure := d.after, d.failure
	d.after, d.failure = "", ""
	var missing []string
	for i, hook := range d.hooks {
		if after != "" && !d.delivered[i] {
			missing = append(missing, hook.Label)
		}
		d.delivered[i] = false
	}
	if len(missing) == 0 {
		return ""
	}
	subject := joinLabels(missing)
	if failure != "" {
		return fmt.Sprintf("%s did not reach this session after its %s: a SessionStart hook failed (%s).", subject, after, failure)
	}
	pronoun := "it"
	if len(missing) > 1 {
		pronoun = "them"
	}
	return fmt.Sprintf("%s did not reach this session after its %s: no SessionStart hook output carried %s. A hook failed, or hooks are turned off in the provider's own settings.", subject, after, pronoun)
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
