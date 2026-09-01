package notify

import (
	"strings"
	"testing"
)

const (
	threadID = "thread-123"
	backend  = "backend-uuid"
)

func thread() ThreadRef { return ThreadRef{ID: threadID, Title: "Fix the parser"} }

// TestEveryKindMapsFromItsMoment walks the four notification-worthy moments
// end to end: what proves each one, what the user is told, and the
// identifier it carries.
func TestEveryKindMapsFromItsMoment(t *testing.T) {
	tests := []struct {
		name  string
		got   func() (Notification, bool)
		kind  Kind
		id    string
		title string
		body  string
		route Target
	}{
		{
			name: "turn complete",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true})
			},
			kind: KindTurnComplete, id: "thread:" + threadID,
			title: "Fix the parser", body: "Completed",
			route: Target{Kind: "thread", ThreadID: threadID},
		},
		{
			name: "turn failed",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true, Failed: true})
			},
			kind: KindError, id: "thread:" + threadID,
			title: "Fix the parser", body: "Failed. Open the thread to see why.",
			route: Target{Kind: "thread", ThreadID: threadID},
		},
		{
			name: "provider exited",
			got: func() (Notification, bool) {
				return MapProviderExit(ProviderExit{Thread: thread()})
			},
			kind: KindError, id: "thread:" + threadID,
			title: "Fix the parser", body: "The provider stopped. Open the thread to see why.",
			route: Target{Kind: "thread", ThreadID: threadID},
		},
		{
			name: "approval needed",
			got: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{
					Thread: thread(), RequestID: "req-7", ToolName: "Bash",
				})
			},
			kind: KindApprovalNeeded, id: "approval:" + threadID + ":req-7",
			title: "Fix the parser", body: "Pending approval: Bash",
			route: Target{Kind: "thread", ThreadID: threadID},
		},
		{
			name: "approval needed with no tool name",
			got: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{Thread: thread(), RequestID: "req-7"})
			},
			kind: KindApprovalNeeded, id: "approval:" + threadID + ":req-7",
			title: "Fix the parser", body: "Pending approval",
			route: Target{Kind: "thread", ThreadID: threadID},
		},
		{
			name: "provider signed out",
			got: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{Provider: "claude", IsSignedOut: true})
			},
			kind: KindProviderSignedOut, id: "provider-auth:claude",
			title: "Claude signed out", body: "Sign in again to keep running turns.",
			route: Target{Kind: "none"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notification, ok := tt.got()
			if !ok {
				t.Fatal("moment produced no notification")
			}
			send := notification.Send
			if send.Retract {
				t.Fatal("a presentation must not be marked as a retraction")
			}
			if send.Kind != tt.kind {
				t.Fatalf("kind = %q, want %q", send.Kind, tt.kind)
			}
			if send.ID != tt.id {
				t.Fatalf("id = %q, want %q", send.ID, tt.id)
			}
			if send.Title != tt.title {
				t.Fatalf("title = %q, want %q", send.Title, tt.title)
			}
			if send.Body != tt.body {
				t.Fatalf("body = %q, want %q", send.Body, tt.body)
			}
			if send.Target != tt.route {
				t.Fatalf("target = %#v, want %#v", send.Target, tt.route)
			}
			if err := ValidateSend(send); err != nil {
				t.Fatalf("mapped notification is not a valid send: %v", err)
			}
		})
	}
}

// TestEveryRetractionMomentWithdrawsItsOwnNotification pairs each presented
// moment with the transition that takes it back, and pins that the two agree
// on the identifier. A retraction naming a different id would leave the
// original on screen forever, which is the exact failure retraction exists
// to prevent.
func TestEveryRetractionMomentWithdrawsItsOwnNotification(t *testing.T) {
	tests := []struct {
		name      string
		presented func() (Notification, bool)
		retracted func() (Notification, bool)
	}{
		{
			name: "resuming a thread withdraws its completion",
			presented: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true})
			},
			retracted: func() (Notification, bool) {
				return MapThreadResumed(ThreadResumed{ThreadID: threadID})
			},
		},
		{
			name: "resuming a thread withdraws its failure",
			presented: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true, Failed: true})
			},
			retracted: func() (Notification, bool) {
				return MapThreadResumed(ThreadResumed{ThreadID: threadID})
			},
		},
		{
			name: "resuming a thread withdraws a provider exit",
			presented: func() (Notification, bool) {
				return MapProviderExit(ProviderExit{Thread: thread()})
			},
			retracted: func() (Notification, bool) {
				return MapThreadResumed(ThreadResumed{ThreadID: threadID})
			},
		},
		{
			name: "answering an approval withdraws its prompt",
			presented: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{Thread: thread(), RequestID: "req-7", ToolName: "Bash"})
			},
			retracted: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{
					Thread: ThreadRef{ID: threadID}, RequestID: "req-7", Answered: true,
				})
			},
		},
		{
			name: "signing back in withdraws the signed-out alert",
			presented: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{Provider: "claude", IsSignedOut: true})
			},
			retracted: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{Provider: "claude", WasSignedOut: true})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			presented, ok := tt.presented()
			if !ok {
				t.Fatal("moment produced no notification")
			}
			retracted, ok := tt.retracted()
			if !ok {
				t.Fatal("retraction moment produced nothing")
			}
			if !retracted.Send.Retract {
				t.Fatal("retraction is not marked as one")
			}
			if retracted.Send.ID != presented.Send.ID {
				t.Fatalf("retraction id = %q, want the presented %q", retracted.Send.ID, presented.Send.ID)
			}
			if retracted.Send.Title != "" || retracted.Send.Body != "" ||
				retracted.Send.Target != (Target{}) {
				t.Fatalf("retraction carries content: %#v", retracted.Send)
			}
			if err := ValidateSend(retracted.Send); err != nil {
				t.Fatalf("retraction is not a valid send: %v", err)
			}
		})
	}
}

// TestOneThreadHoldsOneRestNotification is the anti-stacking contract: three
// different rest moments in one thread share one identifier, so the newest
// fact about the thread REPLACES the previous one instead of adding a second
// alert beside a state that is no longer true.
func TestOneThreadHoldsOneRestNotification(t *testing.T) {
	completed, _ := MapTurnRest(TurnRest{Thread: thread(), TopLevel: true})
	failed, _ := MapTurnRest(TurnRest{Thread: thread(), TopLevel: true, Failed: true})
	exited, _ := MapProviderExit(ProviderExit{Thread: thread()})
	if completed.Send.ID != failed.Send.ID || failed.Send.ID != exited.Send.ID {
		t.Fatalf("rest ids diverge: %q / %q / %q",
			completed.Send.ID, failed.Send.ID, exited.Send.ID)
	}
	if completed.Send.Body == failed.Send.Body {
		t.Fatal("a completed turn and a failed one say the same thing")
	}
}

// TestTwoApprovalsInOneThreadAreSeparatelyRetractable: a subagent and the
// main agent can each be blocked at once, and answering one must not
// withdraw the other's prompt.
func TestTwoApprovalsInOneThreadAreSeparatelyRetractable(t *testing.T) {
	first, _ := MapApproval(ApprovalMoment{Thread: thread(), RequestID: "req-1"})
	second, _ := MapApproval(ApprovalMoment{Thread: thread(), RequestID: "req-2"})
	if first.Send.ID == second.Send.ID {
		t.Fatalf("two approvals share id %q", first.Send.ID)
	}
}

// TestOneProviderSignInDoesNotClearTheOther keeps the two providers'
// identifiers apart.
func TestOneProviderSignInDoesNotClearTheOther(t *testing.T) {
	claude, _ := MapProviderAuth(ProviderAuthChange{Provider: "claude", IsSignedOut: true})
	codex, _ := MapProviderAuth(ProviderAuthChange{Provider: "codex", IsSignedOut: true})
	if claude.Send.ID == codex.Send.ID {
		t.Fatalf("both providers share id %q", claude.Send.ID)
	}
	if codex.Send.Title != "Codex signed out" {
		t.Fatalf("codex title = %q", codex.Send.Title)
	}
}

// TestMomentsThatMustNotNotify walks every case the mapping deliberately
// stays silent for.
func TestMomentsThatMustNotNotify(t *testing.T) {
	tests := []struct {
		name string
		got  func() (Notification, bool)
	}{
		{
			name: "a subagent round is not the user's turn",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: false})
			},
		},
		{
			name: "an interrupt the user performed is not news",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true, Aborted: true})
			},
		},
		{
			name: "a failed turn the user aborted is still their own doing",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: thread(), TopLevel: true, Aborted: true, Failed: true})
			},
		},
		{
			name: "a turn with no thread has nowhere to point",
			got: func() (Notification, bool) {
				return MapTurnRest(TurnRest{Thread: ThreadRef{Title: "orphan"}, TopLevel: true})
			},
		},
		{
			name: "a provider exit with no thread",
			got: func() (Notification, bool) {
				return MapProviderExit(ProviderExit{})
			},
		},
		{
			name: "a resumed thread with no id",
			got: func() (Notification, bool) {
				return MapThreadResumed(ThreadResumed{})
			},
		},
		{
			name: "an approval with no request id cannot be retracted later",
			got: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{Thread: thread()})
			},
		},
		{
			name: "an approval with no thread",
			got: func() (Notification, bool) {
				return MapApproval(ApprovalMoment{RequestID: "req-7"})
			},
		},
		{
			name: "a re-emitted unauthenticated status is not a new transition",
			got: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{
					Provider: "claude", WasSignedOut: true, IsSignedOut: true,
				})
			},
		},
		{
			name: "a sign-in for a provider that was never signed out",
			got: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{Provider: "claude"})
			},
		},
		{
			name: "an auth change naming no provider",
			got: func() (Notification, bool) {
				return MapProviderAuth(ProviderAuthChange{IsSignedOut: true})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if notification, ok := tt.got(); ok {
				t.Fatalf("moment notified anyway: %#v", notification.Send)
			}
		})
	}
}

// TestRedactionKeepsContentOutOfEveryBody is the redaction contract stated
// as a test: whatever a thread is called, and whatever a tool is called, the
// body a notification carries is drawn from this package's own fixed
// phrases. The mapping's input types are what make this true — there is
// nowhere to put an assistant message or a command line — so this pins that
// the phrases themselves never grow one.
func TestRedactionKeepsContentOutOfEveryBody(t *testing.T) {
	secret := "rm -rf /srv/production && curl https://example.invalid/secret"
	loud := ThreadRef{ID: threadID, Title: secret}
	bodies := []string{}
	for _, build := range []func() (Notification, bool){
		func() (Notification, bool) { return MapTurnRest(TurnRest{Thread: loud, TopLevel: true}) },
		func() (Notification, bool) {
			return MapTurnRest(TurnRest{Thread: loud, TopLevel: true, Failed: true})
		},
		func() (Notification, bool) { return MapProviderExit(ProviderExit{Thread: loud}) },
		func() (Notification, bool) {
			return MapApproval(ApprovalMoment{Thread: loud, RequestID: "req-7", ToolName: "Bash"})
		},
	} {
		notification, ok := build()
		if !ok {
			t.Fatal("moment produced no notification")
		}
		bodies = append(bodies, notification.Send.Body)
	}
	for _, body := range bodies {
		if strings.Contains(body, "curl") || strings.Contains(body, "rm -rf") {
			t.Fatalf("body carried thread content: %q", body)
		}
	}
	// The title is the one place a thread's own label may appear, and it is
	// clipped rather than dropped: a notification that names no thread is
	// not worth raising.
	titled, _ := MapTurnRest(TurnRest{Thread: loud, TopLevel: true})
	if !strings.HasPrefix(titled.Send.Title, "rm -rf") {
		t.Fatalf("title = %q, want the thread's own label", titled.Send.Title)
	}
}

// TestTitleAndBodyAreBoundedAndSingleLine covers the sanitizer that stands
// between untrusted text and a notification daemon.
func TestTitleAndBodyAreBoundedAndSingleLine(t *testing.T) {
	messy := ThreadRef{
		ID:    threadID,
		Title: "  line one\nline\ttwo\x00\x1b  " + strings.Repeat("padding ", 40),
	}
	notification, ok := MapTurnRest(TurnRest{Thread: messy, TopLevel: true})
	if !ok {
		t.Fatal("moment produced no notification")
	}
	title := notification.Send.Title
	if strings.ContainsAny(title, "\n\t\x00\x1b") {
		t.Fatalf("title carries control characters: %q", title)
	}
	if runes := len([]rune(title)); runes > MaxTitleRunes {
		t.Fatalf("title is %d runes, want at most %d", runes, MaxTitleRunes)
	}
	if !strings.HasSuffix(title, "...") {
		t.Fatalf("clipped title = %q, want a visible ellipsis", title)
	}
	if !strings.HasPrefix(title, "line one line two ") {
		t.Fatalf("title = %q, want collapsed whitespace and dropped controls", title)
	}
}

// TestAnUntitledThreadStillNotifies: a thread whose row is gone by the time
// the notification is composed still gets one, with the label the app's own
// database defaults to.
func TestAnUntitledThreadStillNotifies(t *testing.T) {
	notification, ok := MapTurnRest(TurnRest{Thread: ThreadRef{ID: threadID}, TopLevel: true})
	if !ok {
		t.Fatal("an untitled thread produced no notification")
	}
	if notification.Send.Title != UntitledThread {
		t.Fatalf("title = %q, want %q", notification.Send.Title, UntitledThread)
	}
}

// TestToolNameIsClippedNotDropped: a tool name is the one provider-supplied
// token a body carries, and it is bounded like everything else that crosses
// the redaction line.
func TestToolNameIsClippedNotDropped(t *testing.T) {
	notification, ok := MapApproval(ApprovalMoment{
		Thread: thread(), RequestID: "req-7",
		ToolName: "mcp__" + strings.Repeat("server", 20) + "__tool",
	})
	if !ok {
		t.Fatal("moment produced no notification")
	}
	body := notification.Send.Body
	if !strings.HasPrefix(body, "Pending approval: mcp__server") {
		t.Fatalf("body = %q", body)
	}
	if runes := len([]rune(body)); runes > MaxBodyRunes {
		t.Fatalf("body is %d runes, want at most %d", runes, MaxBodyRunes)
	}
}

func TestSummaryLine(t *testing.T) {
	tests := []struct {
		name  string
		value string
		max   int
		want  string
	}{
		{name: "empty", value: "", max: 10, want: ""},
		{name: "whitespace only", value: " \n\t ", max: 10, want: ""},
		{name: "collapses runs", value: "a   b\n\nc", max: 10, want: "a b c"},
		{name: "trims edges", value: "  hello  ", max: 10, want: "hello"},
		{name: "drops controls without splitting", value: "he\x00llo", max: 10, want: "hello"},
		{name: "keeps unicode", value: "héllo ✓", max: 10, want: "héllo ✓"},
		{name: "clips with ellipsis", value: "abcdefghij", max: 6, want: "abc..."},
		{name: "no budget", value: "abc", max: 0, want: ""},
		{name: "budget under the ellipsis", value: "abcdef", max: 2, want: "ab"},
		{name: "does not clip mid-space", value: "ab cdefgh", max: 6, want: "ab..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SummaryLine(tt.value, tt.max); got != tt.want {
				t.Fatalf("SummaryLine(%q, %d) = %q, want %q", tt.value, tt.max, got, tt.want)
			}
		})
	}
}

func TestProviderDisplayName(t *testing.T) {
	for value, want := range map[string]string{
		"claude":     "Claude",
		"claude-tui": "Claude",
		"codex":      "Codex",
		"cursor":     "cursor",
		"":           "",
	} {
		if got := ProviderDisplayName(value); got != want {
			t.Fatalf("ProviderDisplayName(%q) = %q, want %q", value, got, want)
		}
	}
}

// TestKnownKindRefusesAnythingUndeclared: the preference gate switches on
// the kind, so an undeclared one is a preference nobody can express.
func TestKnownKindRefusesAnythingUndeclared(t *testing.T) {
	for _, kind := range []Kind{
		KindTurnComplete, KindApprovalNeeded, KindError,
		KindProviderSignedOut, KindWorkflowAttention, KindAppUpdate,
	} {
		if !KnownKind(kind) {
			t.Fatalf("declared kind %q is not known", kind)
		}
	}
	for _, kind := range []Kind{"", "turn_complete", "TurnComplete", "push"} {
		if KnownKind(kind) {
			t.Fatalf("undeclared kind %q was admitted", kind)
		}
	}
}

// TestBackendIDAttributesWithoutRouting is deliverable 3's contract in one
// place: the backend id composes with every route kind (it answers "whose",
// not "where"), it survives the platform round trip, and it is bounded.
func TestBackendIDAttributesWithoutRouting(t *testing.T) {
	for _, target := range []Target{
		{Kind: "thread", ThreadID: threadID, BackendID: backend},
		{Kind: "workflow-item", WorkItemID: "item-1", BackendID: backend},
		{Kind: "none", BackendID: backend},
	} {
		t.Run(target.Kind, func(t *testing.T) {
			if err := ValidateTarget(target); err != nil {
				t.Fatalf("backend id refused on a %s target: %v", target.Kind, err)
			}
			data, err := TargetToMap(target)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := TargetFromMap(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != target {
				t.Fatalf("round trip = %#v, want %#v", got, target)
			}
		})
	}
	oversized := Target{Kind: "none", BackendID: strings.Repeat("x", MaxBackendIDBytes+1)}
	if err := ValidateTarget(oversized); err == nil {
		t.Fatal("an unbounded backend id was admitted")
	}
	// The one-id rule is untouched by the new field: a route carrying two
	// route ids is still refused, backend id or not.
	both := Target{Kind: "thread", ThreadID: threadID, WorkItemID: "item-1", BackendID: backend}
	if err := ValidateTarget(both); err == nil {
		t.Fatal("adding a backend id let a two-route target through")
	}
}

func TestValidateSend(t *testing.T) {
	valid := Send{
		ID: "thread:" + threadID, Kind: KindTurnComplete,
		Title: "Fix the parser", Body: "Completed",
		Target: Target{Kind: "thread", ThreadID: threadID},
	}
	if err := ValidateSend(valid); err != nil {
		t.Fatalf("valid send refused: %v", err)
	}
	retraction := Send{ID: valid.ID, Kind: KindTurnComplete, Retract: true}
	if err := ValidateSend(retraction); err != nil {
		t.Fatalf("valid retraction refused: %v", err)
	}

	tests := []struct {
		name string
		send Send
	}{
		{name: "no id", send: Send{Kind: KindTurnComplete, Title: "t", Target: Target{Kind: "none"}}},
		{
			name: "oversized id",
			send: Send{
				ID: strings.Repeat("x", MaxIDBytes+1), Kind: KindTurnComplete,
				Title: "t", Target: Target{Kind: "none"},
			},
		},
		{name: "no kind", send: Send{ID: "a", Title: "t", Target: Target{Kind: "none"}}},
		{name: "unknown kind", send: Send{ID: "a", Kind: "gossip", Title: "t", Target: Target{Kind: "none"}}},
		{name: "no title", send: Send{ID: "a", Kind: KindTurnComplete, Target: Target{Kind: "none"}}},
		{
			name: "oversized title",
			send: Send{
				ID: "a", Kind: KindTurnComplete,
				Title: strings.Repeat("x", MaxTitleBytes+1), Target: Target{Kind: "none"},
			},
		},
		{
			name: "oversized body",
			send: Send{
				ID: "a", Kind: KindTurnComplete, Title: "t",
				Body: strings.Repeat("x", MaxBodyBytes+1), Target: Target{Kind: "none"},
			},
		},
		{name: "invalid target", send: Send{ID: "a", Kind: KindTurnComplete, Title: "t"}},
		{
			name: "retraction carrying a title",
			send: Send{ID: "a", Kind: KindTurnComplete, Retract: true, Title: "t"},
		},
		{
			name: "retraction carrying a route",
			send: Send{
				ID: "a", Kind: KindTurnComplete, Retract: true,
				Target: Target{Kind: "thread", ThreadID: threadID},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSend(tt.send); err == nil {
				t.Fatalf("validate %#v unexpectedly succeeded", tt.send)
			}
		})
	}
}
