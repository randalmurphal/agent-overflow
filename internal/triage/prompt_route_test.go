package triage

import (
	"slices"
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

// open names the prompt open on threadID under requestID.
func (f *promptRouteFixture) open(t *testing.T, threadID, requestID string) PromptRef {
	t.Helper()
	f.router.mu.Lock()
	defer f.router.mu.Unlock()
	_, occurrence, _, open := f.router.openPromptLocked(threadID, requestID)
	if !open {
		t.Fatalf("no prompt open on %s under %s", threadID, requestID)
	}
	return PromptRef{ThreadID: threadID, RequestID: requestID, Occurrence: occurrence}
}

// A request id does not name a prompt: a session can ask again under an id
// it resolved, and a replacement session under its predecessor's. Each
// prompt is its own occurrence, and one of a session that no longer holds
// the thread is not open, whatever was asked under the same id since.
func TestAPromptIsOpenOnlyAsTheOccurrenceThatAskedIt(t *testing.T) {
	t.Parallel()
	f := newPromptRouteFixture(t)
	f.route("worker", "lead")
	f.router.MarkThreadActive("worker", "s1")
	f.approval(t, "worker", "req", "s1")
	first := f.open(t, "worker", "req")
	f.wantRoute(t, first, true, "lead")

	// The same session resolves it and asks again under the same id.
	if err := f.router.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: "worker", ItemID: "req", SessionToken: "s1",
		Meta: mustMarshalJSON(t, map[string]any{"requestId": "req", "decision": "approved"}), Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	f.approval(t, "worker", "req", "s1")
	again := f.open(t, "worker", "req")
	f.wantRoute(t, first, false, "")
	f.wantRoute(t, again, true, "lead")

	// A start that replaces the live session keeps the thread's state, and
	// the first session's prompt with it.
	f.router.MarkThreadActive("worker", "s2")
	f.wantRoute(t, again, false, "")
	f.approval(t, "worker", "req", "s2")
	second := f.open(t, "worker", "req")
	f.wantRoute(t, again, false, "")
	f.wantRoute(t, second, true, "lead")

	f.router.CleanupThread("worker")
	f.wantRoute(t, second, false, "")
	f.router.MarkThreadActive("worker", "s3")
	f.question(t, "worker", "req", "s3")
	f.wantRoute(t, second, false, "")
	f.wantRoute(t, f.open(t, "worker", "req"), true, "lead")
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
	handed, orphan, other := f.open(t, "handed", "approve"), f.open(t, "orphan", "ask"), f.open(t, "other", "approve-other")

	// Narrowed to an earlier prompt under the same id, nothing moves.
	stale := handed
	stale.Occurrence--
	if moved := f.router.ReroutePrompts("lead", stale); moved != 0 {
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
		if got := f.announced[requestID]; !slices.Equal(got, want) {
			t.Errorf("%s announced with routes %q, want %q", requestID, got, want)
		}
	}
}
