package triage

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// promptRouteFixture is a router whose prompt router answers from routes,
// recording the request announcements it emits.
type promptRouteFixture struct {
	router *Router
	mu     sync.Mutex
	routes map[string]string
	// announced is the route of each request announcement, by request id;
	// "" is the user.
	announced map[string][]string
}

func newPromptRouteFixture(t *testing.T) *promptRouteFixture {
	t.Helper()
	f := &promptRouteFixture{routes: map[string]string{}, announced: map[string][]string{}}
	f.router = NewRouter(nil, func(name eventchan.Channel, data any) {
		var requestID string
		var route *provider.PromptRoute
		switch event := data.(type) {
		case provider.ApprovalEvent:
			if event.Action != "request" {
				return
			}
			requestID, route = event.Request.RequestID, event.Request.RoutedTo
		case provider.UserInputEvent:
			if event.Action != "request" {
				return
			}
			requestID, route = event.Request.RequestID, event.Request.RoutedTo
		default:
			return
		}
		to := ""
		if route != nil {
			to = route.ThreadID
		}
		f.mu.Lock()
		f.announced[requestID] = append(f.announced[requestID], to)
		f.mu.Unlock()
	})
	f.router.SetPromptRouter(func(threadID string) *provider.PromptRoute {
		f.mu.Lock()
		defer f.mu.Unlock()
		if to := f.routes[threadID]; to != "" {
			return &provider.PromptRoute{ThreadID: to, Title: to}
		}
		return nil
	})
	return f
}

func (f *promptRouteFixture) route(threadID, to string) {
	f.mu.Lock()
	f.routes[threadID] = to
	f.mu.Unlock()
}

func (f *promptRouteFixture) approval(t *testing.T, threadID, requestID, session string) {
	t.Helper()
	meta := mustMarshalJSON(t, provider.ApprovalRequest{RequestID: requestID, ThreadID: threadID, ToolName: "Bash"})
	if err := f.router.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: threadID, ItemID: requestID, Meta: meta, Timestamp: time.Now(), SessionToken: session,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *promptRouteFixture) question(t *testing.T, threadID, requestID, session string) {
	t.Helper()
	meta := mustMarshalJSON(t, provider.UserInputRequest{
		RequestID: requestID, ThreadID: threadID, ToolName: "user_input",
		Questions: []provider.UserInputQuestion{{ID: "q", Header: "Scope", Question: "Which?"}},
	})
	if err := f.router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserInputRequest, ThreadID: threadID, ItemID: requestID, Meta: meta, Timestamp: time.Now(), SessionToken: session,
	}); err != nil {
		t.Fatal(err)
	}
}

// wantRoute checks what PromptRoute says of ref: open, and routed to to
// ("" is the user).
func (f *promptRouteFixture) wantRoute(t *testing.T, ref PromptRef, open bool, to string) {
	t.Helper()
	route, gotOpen := f.router.PromptRoute(ref)
	got := ""
	if route != nil {
		got = route.ThreadID
	}
	if gotOpen != open || got != to {
		t.Fatalf("PromptRoute(%+v) = %q open %v, want %q open %v", ref, got, gotOpen, to, open)
	}
}

// A request id names a prompt only within the session that asked it: a
// session a replacement took the thread from, with or without a cleanup,
// has no open prompt, whatever the replacement asked under the same id.
func TestAPromptIsOpenOnlyInTheSessionThatAskedIt(t *testing.T) {
	t.Parallel()
	f := newPromptRouteFixture(t)
	f.route("worker", "lead")
	f.router.MarkThreadActive("worker", "s1")
	f.approval(t, "worker", "req", "s1")
	first := PromptRef{ThreadID: "worker", RequestID: "req", SessionToken: "s1"}
	f.wantRoute(t, first, true, "lead")

	// A start that replaces the live session keeps the thread's state, and
	// the first session's prompt with it.
	f.router.MarkThreadActive("worker", "s2")
	f.wantRoute(t, first, false, "")
	f.approval(t, "worker", "req", "s2")
	second := PromptRef{ThreadID: "worker", RequestID: "req", SessionToken: "s2"}
	f.wantRoute(t, first, false, "")
	f.wantRoute(t, second, true, "lead")

	f.router.CleanupThread("worker")
	f.wantRoute(t, second, false, "")
	f.router.MarkThreadActive("worker", "s3")
	f.question(t, "worker", "req", "s3")
	f.wantRoute(t, second, false, "")
	f.wantRoute(t, PromptRef{ThreadID: "worker", RequestID: "req", SessionToken: "s3"}, true, "lead")
}

// ReroutePrompts moves every open prompt routed to a thread to where the
// prompt router routes its worker now, to the user when that is the same
// thread, announces each once, and leaves alone the prompts routed
// elsewhere, the ones refs does not name and the ones already moved.
func TestReroutePromptsMovesThePromptsOfOneRecipient(t *testing.T) {
	t.Parallel()
	f := newPromptRouteFixture(t)
	for worker, to := range map[string]string{"handed": "lead", "orphan": "lead", "other": "elsewhere"} {
		f.route(worker, to)
		f.router.MarkThreadActive(worker, "s-"+worker)
	}
	f.approval(t, "handed", "approve", "s-handed")
	f.question(t, "orphan", "ask", "s-orphan")
	f.approval(t, "other", "approve-other", "s-other")
	handed := PromptRef{ThreadID: "handed", RequestID: "approve", SessionToken: "s-handed"}
	orphan := PromptRef{ThreadID: "orphan", RequestID: "ask", SessionToken: "s-orphan"}
	other := PromptRef{ThreadID: "other", RequestID: "approve-other", SessionToken: "s-other"}

	// Narrowed to a prompt of a session that did not ask it, nothing moves.
	if moved := f.router.ReroutePrompts("lead", PromptRef{ThreadID: "handed", RequestID: "approve", SessionToken: "s-old"}); moved != 0 {
		t.Fatalf("a stale ref moved %d prompts", moved)
	}
	f.route("handed", "next")
	if moved := f.router.ReroutePrompts("lead"); moved != 2 {
		t.Fatalf("moved %d prompts, want 2", moved)
	}
	f.wantRoute(t, handed, true, "next")
	f.wantRoute(t, orphan, true, "")
	f.wantRoute(t, other, true, "elsewhere")
	if moved := f.router.ReroutePrompts("lead"); moved != 0 {
		t.Fatalf("a second call moved %d prompts", moved)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for requestID, want := range map[string][]string{"approve": {"lead", "next"}, "ask": {"lead", ""}, "approve-other": {"elsewhere"}} {
		if got := f.announced[requestID]; len(got) != len(want) || got[len(got)-1] != want[len(want)-1] {
			t.Errorf("%s announced with routes %q, want %q", requestID, got, want)
		}
	}
}
