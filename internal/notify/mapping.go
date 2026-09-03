package notify

import (
	"strings"
	"unicode"
)

// The event→notification mapping (docs/specs/remote-access.md §9,
// "Notification semantics"). Four moments are worth interrupting a person
// for, and this file is the whole of the decision: which internal transition
// proves each one, what the user is told, what identifier the moment carries,
// and when a later transition takes it back.
//
// PURE, AND DELIBERATELY UNDER-INFORMED. Every entry point takes a small
// purpose-built input and returns a Notification, with no store, no App and
// no clock. The inputs are the redaction policy made structural rather than
// promised: a notification body cannot carry an assistant message, a tool's
// output, a command line or a provider's error prose, because the types the
// mapping accepts have nowhere to put them. §9 wants that policy for push
// (payloads transit Apple and Google), and a desktop notification lands on a
// lock screen a room full of people can read, so the same line holds for
// both. What a notification may say is a bounded thread TITLE — the label the
// user chose or the app generated for the thread, already visible in the
// sidebar — plus a fixed phrase from this file.
//
// One consequence worth stating: the mapping cannot explain a failure, only
// report one. That is the intended trade. The detail is one click away in the
// thread, which is where the click goes.
//
// RETRACTION, NOT PRESENCE-GUESSING. Each moment has a stable ID, so the
// second notification about a thread REPLACES the first rather than stacking
// beside it, and the transition that ends the moment retracts it. §9 chose
// this over presence heuristics on purpose: "the desktop is attached but
// unattended" is exactly the case a presence guess gets wrong, and it is the
// common one. A retract for an ID that was never presented is a no-op
// everywhere, so nothing has to remember what it showed.

// Kind names one notification-worthy moment. It rides the wire on Send
// because preferences are per event type (§9) and the presenter applying them
// is not always this process.
type Kind string

const (
	// KindTurnComplete is the agent finishing a top-level turn: the thread
	// is resting and there is something to read.
	KindTurnComplete Kind = "turn-complete"
	// KindApprovalNeeded is the agent blocked on the user's permission. The
	// only one of the four where work has stopped and stays stopped until a
	// person acts, which is why §6's default has it on.
	KindApprovalNeeded Kind = "approval-needed"
	// KindError is a turn that ended badly, or a provider process that died
	// under a thread. It shares KindTurnComplete's identifier because both
	// describe the same fact — this thread stopped — and a thread stops once.
	KindError Kind = "error"
	// KindProviderSignedOut is a provider whose login is gone. Account-wide,
	// not thread-scoped: nothing will run for anyone until it is fixed.
	KindProviderSignedOut Kind = "provider-signed-out"
	// KindWorkflowAttention is a workflow item that needs a human or failed.
	// It predates this mapping (App.notifyOS's first production sender) and
	// is named here so every send carries a kind and the preference gate is
	// total. It has a toggle of its own like every other kind: a moment
	// nobody can silence individually is one whose only answer is the master
	// switch, which is the wrong price for one noisy workflow.
	KindWorkflowAttention Kind = "workflow-attention"
	// KindAppUpdate is the WSL launcher's "update didn't apply" notice, the
	// second sender that predates this mapping. Its own toggle too, same
	// reasoning as KindWorkflowAttention.
	KindAppUpdate Kind = "app-update"
)

// kinds is every declared Kind. A send naming anything else is refused by
// ValidateSend rather than presented, because an unknown kind is a
// preference nobody can express: the gate would have to either silence a
// moment the user never asked to silence or raise one they did.
var kinds = map[Kind]struct{}{
	KindTurnComplete:      {},
	KindApprovalNeeded:    {},
	KindError:             {},
	KindProviderSignedOut: {},
	KindWorkflowAttention: {},
	KindAppUpdate:         {},
}

// KnownKind reports whether kind is one this build declares.
func KnownKind(kind Kind) bool {
	_, ok := kinds[kind]
	return ok
}

// kindPhrases is what each moment is called when the thread it happened to
// may not be named — one fixed phrase per kind, no variable part at all.
//
// THE PHONE PUSH IS WHAT THIS EXISTS FOR. A desktop notification names the
// thread in its title, because it renders on the machine the thread lives
// on. A push transits Google (§9's redaction rule), so the phone is told
// which MOMENT happened and which machine is asking, and it fetches the
// rest over the paired session once the person taps. `internal/push` reads
// this; nothing else may compose a phrase of its own, or two surfaces would
// call one moment two things.
//
// TOTAL over `kinds`, and TestKindPhraseCoversEveryKind fails when a new
// kind arrives without one: a moment with no phrase would push an empty
// title, which reads on a lock screen as a notification from nothing.
var kindPhrases = map[Kind]string{
	KindTurnComplete:      "Turn complete",
	KindApprovalNeeded:    "Approval needed",
	KindError:             "Turn failed",
	KindProviderSignedOut: "Provider signed out",
	KindWorkflowAttention: "Workflow needs you",
	KindAppUpdate:         "Update notice",
}

// KindPhrase is what a notification says when it may not name its thread.
// An undeclared kind answers "", which ValidateSend has already refused.
func KindPhrase(kind Kind) string { return kindPhrases[kind] }

// Notification is one mapped moment: the wire payload, and nothing else.
//
// Kind lives on Send rather than beside it because the preference gate and
// the remote presenter both need it, and a value that has to travel is a
// field, not a return tuple.
type Notification struct {
	Send Send
}

// Text budgets. Far below Send's wire ceilings, and that gap is the point:
// the wire limits bound what a buggy producer can push through the ring,
// while these bound what a person is shown. Notification surfaces truncate
// silently and differently per platform, so text that survives to the
// presenter unclipped is text the user actually reads.
const (
	// MaxTitleRunes fits a thread title on one line of a macOS banner, a
	// GNOME notification and a Windows toast without any of them eliding it
	// mid-word.
	MaxTitleRunes = 80
	// MaxBodyRunes bounds the fixed phrase plus the one variable token a
	// body ever carries (a tool name).
	MaxBodyRunes = 120
	// MaxToolNameRunes bounds the one provider-supplied token that reaches a
	// body. A tool NAME is not content — it names a capability, not what the
	// capability was asked to do — but it is still provider-supplied text,
	// so it is clipped like everything else that crosses this line.
	MaxToolNameRunes = 40
)

// UntitledThread is what a notification calls a thread with no title. It is
// the same words the database defaults a thread's title column to, so a
// notification never invents a name the sidebar does not show.
const UntitledThread = "New Thread"

// ThreadRef is a thread reduced to what a notification may know about it: an
// id to route the click to, and the label the sidebar already shows.
//
// Deliberately not store.Thread. Handing the mapping the row would hand it
// the workspace path, the last user message's anchor and the live todo list,
// and the redaction policy would then be a habit rather than a shape.
type ThreadRef struct {
	ID    string
	Title string
}

// TurnRest is a turn arriving at rest — the `provider:turn_completed`
// transition, which is the only thing in the tree that proves a turn ended.
type TurnRest struct {
	Thread ThreadRef
	// TopLevel is false for a subagent's round. A subagent finishing is not
	// the user's turn finishing; the parent turn is still running and its
	// own completion is the moment worth reporting.
	TopLevel bool
	// Failed is a turn that ended carrying an error. It maps to KindError
	// rather than KindTurnComplete: one turn ending is one moment, and
	// "finished" and "failed" are two answers to the same question. Sending
	// both would put two notifications on screen for one event.
	Failed bool
	// Aborted is the user interrupting. Nothing is notified — the person who
	// pressed stop is looking at the screen, and telling them what they just
	// did is noise. It is separate from Failed because the wire event is:
	// an abort carries no error and a failure is not something anyone asked
	// for.
	Aborted bool
}

// ProviderExit is the provider process dying under a thread — the
// `provider:session_died` transition. A distinct input from TurnRest because
// it can arrive with no turn in flight at all, and the sentence differs.
type ProviderExit struct {
	Thread ThreadRef
}

// ThreadResumed is a thread going back to work — the
// `provider:turn_started` transition. It carries no rest of its own; it is
// the retraction moment for whatever rest notification the thread was
// holding. The user starting the next turn IS the "handled elsewhere" §9
// asks retraction to cover.
type ThreadResumed struct {
	ThreadID string
}

// ApprovalMoment is an approval request or its answer — the two actions of
// the `provider:approval` transition.
type ApprovalMoment struct {
	Thread    ThreadRef
	RequestID string
	// ToolName is the capability being asked for ("Bash", "Edit"). The
	// request's description, title and input are deliberately absent: §9
	// calls command text sensitive by name, and a notification is the one
	// surface that renders outside the app's own window.
	ToolName string
	// Answered is the resolve/fail action. Whoever answered — this device,
	// another device, or the provider giving up on the request — the prompt
	// is gone, so the notification about it must go too.
	Answered bool
}

// ProviderAuthChange is a provider's login crossing the line, in either
// direction.
//
// BOTH edges are parameters, so the mapping stays pure and the caller owns
// the two booleans of state this needs. That is not ceremony: the
// `provider:status` unauthenticated event is a LEVEL, re-emitted every time
// anything asks for provider statuses, and mapping it directly would raise a
// fresh alert every time the settings page loaded.
type ProviderAuthChange struct {
	// Provider is the wire name ("claude", "codex").
	Provider     string
	WasSignedOut bool
	IsSignedOut  bool
}

// threadNotificationID is the identifier shared by every "this thread
// stopped" notification: turn complete, turn failed, provider exit.
//
// One id for three moments is the retraction contract, not a shortcut. A
// thread is in exactly one rest state at a time, so a later rest REPLACES
// the earlier one — a turn that fails after one that succeeded leaves
// "Failed" on screen, not both — and one retraction on resume clears
// whichever of the three is showing without the retracting side having to
// know which it was.
func threadNotificationID(threadID string) string { return "thread:" + threadID }

// approvalNotificationID is per REQUEST, not per thread: two approvals can
// be outstanding in one thread (a subagent's and the main agent's), each
// answered on its own, so each needs an identifier that can be retracted on
// its own.
func approvalNotificationID(threadID, requestID string) string {
	return "approval:" + threadID + ":" + requestID
}

// providerAuthNotificationID is per provider. Claude being signed out says
// nothing about Codex, and one id for both would let the second sign-in
// clear the first provider's alert.
func providerAuthNotificationID(providerName string) string {
	return "provider-auth:" + providerName
}

// MapTurnRest maps a settled turn. Reports false when the moment is not the
// user's to hear about: a subagent's round, or an abort they performed.
func MapTurnRest(rest TurnRest) (Notification, bool) {
	if rest.Thread.ID == "" || !rest.TopLevel || rest.Aborted {
		return Notification{}, false
	}
	kind, body := KindTurnComplete, "Completed"
	if rest.Failed {
		kind, body = KindError, "Failed. Open the thread to see why."
	}
	return threadNotification(kind, rest.Thread, body), true
}

// MapProviderExit maps a provider process dying under a thread.
func MapProviderExit(exit ProviderExit) (Notification, bool) {
	if exit.Thread.ID == "" {
		return Notification{}, false
	}
	return threadNotification(
		KindError, exit.Thread, "The provider stopped. Open the thread to see why.",
	), true
}

// MapThreadResumed retracts a thread's rest notification. The kind on a
// retraction is KindTurnComplete because that is what the overwhelming
// majority of retracted rest notifications are, and because a retraction's
// kind decides only which preference gate it passes: a user who silenced
// turn completions has no rest notification of this shape to withdraw, and a
// user who silenced errors keeps the gate they asked for either way.
func MapThreadResumed(resumed ThreadResumed) (Notification, bool) {
	if resumed.ThreadID == "" {
		return Notification{}, false
	}
	return Notification{Send: Send{
		ID:      threadNotificationID(resumed.ThreadID),
		Kind:    KindTurnComplete,
		Retract: true,
	}}, true
}

// MapApproval maps an approval request to a notification and its answer to
// that notification's retraction.
func MapApproval(moment ApprovalMoment) (Notification, bool) {
	if moment.Thread.ID == "" || moment.RequestID == "" {
		return Notification{}, false
	}
	id := approvalNotificationID(moment.Thread.ID, moment.RequestID)
	if moment.Answered {
		return Notification{Send: Send{ID: id, Kind: KindApprovalNeeded, Retract: true}}, true
	}
	body := "Pending approval"
	if tool := SummaryLine(moment.ToolName, MaxToolNameRunes); tool != "" {
		body += ": " + tool
	}
	notification := threadNotification(KindApprovalNeeded, moment.Thread, body)
	notification.Send.ID = id
	return notification, true
}

// MapProviderAuth maps a provider's login crossing the line. Reports false
// on a level that did not change, which is what keeps a re-emitted
// unauthenticated status from re-alerting.
func MapProviderAuth(change ProviderAuthChange) (Notification, bool) {
	name := SummaryLine(change.Provider, MaxToolNameRunes)
	if name == "" || change.WasSignedOut == change.IsSignedOut {
		return Notification{}, false
	}
	id := providerAuthNotificationID(change.Provider)
	if !change.IsSignedOut {
		return Notification{Send: Send{ID: id, Kind: KindProviderSignedOut, Retract: true}}, true
	}
	return Notification{Send: Send{
		ID:    id,
		Kind:  KindProviderSignedOut,
		Title: ProviderDisplayName(change.Provider) + " signed out",
		Body:  "Sign in again to keep running turns.",
		// No route: a provider's login is not a thread and not a workflow
		// item. The click foregrounds the app, where the sign-in banner is.
		Target: Target{Kind: TargetNone},
	}}, true
}

// threadNotification is the shape every thread-scoped moment shares: the
// thread's own title as the heading, a fixed phrase as the body, and a click
// that opens that thread.
//
// Title carries the thread and body carries the state, matching the workflow
// sender that already ships (the run's goal as title, what it needs as body)
// and the sidebar row it mirrors. The heading is what a person scans when
// four threads are running.
func threadNotification(kind Kind, thread ThreadRef, body string) Notification {
	title := SummaryLine(thread.Title, MaxTitleRunes)
	if title == "" {
		title = UntitledThread
	}
	return Notification{Send: Send{
		ID:     threadNotificationID(thread.ID),
		Kind:   kind,
		Title:  title,
		Body:   SummaryLine(body, MaxBodyRunes),
		Target: Target{Kind: TargetThread, ThreadID: thread.ID},
	}}
}

// ProviderDisplayName is what a notification calls a provider. Unknown names
// pass through sanitized rather than being dropped: a provider this build
// has no label for is still one the user has to sign back into.
func ProviderDisplayName(providerName string) string {
	switch providerName {
	case "claude", "claude-tui":
		return "Claude"
	case "codex":
		return "Codex"
	default:
		return SummaryLine(providerName, MaxToolNameRunes)
	}
}

// SummaryLine reduces text to one bounded, presentable line. It is the
// redaction boundary's second half: the input types decide WHAT may be said,
// and this decides how much of it and in what shape.
//
// Three jobs, all of them load-bearing at a notification surface:
//
//   - Non-printing runes are DROPPED, not replaced. A thread title carrying
//     a NUL cannot be sent over D-Bus at all (the string type forbids it),
//     and an ANSI escape or a bidi override in a heading renders as
//     something other than what it says.
//   - Whitespace collapses to single spaces. A notification body is one
//     line whatever the source did; a title with a newline in it either
//     truncates at the newline or grows the banner, depending on the
//     platform.
//   - The cut is by RUNE with an ellipsis, so the result reads as clipped
//     rather than as a shorter title somebody chose.
func SummaryLine(value string, maxRunes int) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			space = builder.Len() > 0
		case !unicode.IsPrint(r):
			// Dropped entirely, and without marking a space: a control
			// character between two words is not a word boundary.
		default:
			if space {
				builder.WriteRune(' ')
				space = false
			}
			builder.WriteRune(r)
		}
	}
	return clipRunes(builder.String(), maxRunes)
}

// clipRunes cuts at maxRunes runes, spending three of them on the ellipsis
// so the result never exceeds the budget it was given.
func clipRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimRight(string(runes[:maxRunes-3]), " ") + "..."
}
