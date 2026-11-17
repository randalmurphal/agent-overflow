package browser

import (
	"context"
	"testing"
	"time"
)

// agentTransitions answers the page's Agent flag in each state event the
// log holds now, keeping only the changes.
func agentTransitions(log *eventLog, pageID string, last bool) []bool {
	var changes []bool
	for {
		select {
		case event := <-log.ch:
			if event.Kind != "state" {
				continue
			}
			for _, page := range event.Pages {
				if page.ID == pageID && page.Agent != last {
					last = page.Agent
					changes = append(changes, last)
				}
			}
		default:
			return changes
		}
	}
}

func agentShown(m *Manager, access Access, pageID string) bool {
	tab, _ := findTab(m, access, pageID)
	return tab.Agent
}

// An agent's run of tool calls shows on the page's tab as one transition
// in, and the user's own actions never do.
func TestAgentActivityShowsOnTransitionsOnly(t *testing.T) {
	m, _, access, firstID, secondID := newPaneHostManager(t)
	m.agentLinger = time.Hour
	log := recordEvents(m)

	if _, err := m.Open(t.Context(), access, "https://example.test/user", OpenOptions{PageID: firstID}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatal(err)
	}
	if agentShown(m, access, firstID) {
		t.Fatal("a user action showed agent activity")
	}

	for i := range 3 {
		ctx, end := withAgentCall(t.Context())
		if _, err := m.Open(ctx, access, "https://example.test/agent", OpenOptions{PageID: firstID}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !agentShown(m, access, firstID) {
			t.Fatalf("call %d did not show on the tab", i)
		}
		end()
	}
	if got := agentTransitions(log, firstID, false); len(got) != 1 || !got[0] {
		t.Fatalf("Agent transitions = %v, want one in for the whole run", got)
	}

	// The same run as the user's and as an agent's: the agent's pushes the
	// state once more, for its one transition, not once per call.
	countStates := func(ctx func() (context.Context, func())) int {
		n := 0
		m.SetEventSink(func(event CompanionEvent) {
			if event.Kind == "state" {
				n++
			}
		})
		for range 3 {
			c, end := ctx()
			if _, err := m.Open(c, access, "https://example.test/count", OpenOptions{PageID: secondID}); err != nil {
				t.Fatal(err)
			}
			end()
		}
		m.SetEventSink(nil)
		return n
	}
	user := countStates(func() (context.Context, func()) { return t.Context(), func() {} })
	agent := countStates(func() (context.Context, func()) { return withAgentCall(t.Context()) })
	if agent != user+1 {
		t.Fatalf("an agent's run of 3 calls pushed %d states, the user's %d; want exactly one more", agent, user)
	}
	// Activity shows; it gates nothing.
	if err := m.ReloadCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatalf("the user's reload during agent activity: %v", err)
	}
}

// The activity clears once the last call out has lingered, and not while
// another call is still in the page.
func TestAgentActivityClearsAfterTheLastCallOut(t *testing.T) {
	m, _, access, firstID, secondID := newPaneHostManager(t)
	m.agentLinger = 10 * time.Millisecond
	log := recordEvents(m)

	ctxA, endA := withAgentCall(t.Context())
	ctxB, endB := withAgentCall(t.Context())
	for _, ctx := range []context.Context{ctxA, ctxB} {
		if _, err := m.Open(ctx, access, "https://example.test/agent", OpenOptions{PageID: firstID}); err != nil {
			t.Fatal(err)
		}
	}
	endA()
	time.Sleep(60 * time.Millisecond)
	if !agentShown(m, access, firstID) {
		t.Fatal("the activity cleared while a call was still in the page")
	}
	endB()
	endB() // a repeated end leaves nothing twice
	var got []bool
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		last := len(got) > 0 && got[len(got)-1]
		got = append(got, agentTransitions(log, firstID, last)...)
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("Agent transitions = %v, want in then out", got)
	}
	if agentShown(m, access, firstID) {
		t.Fatal("the state still shows the activity")
	}
	if agentShown(m, access, secondID) {
		t.Fatal("a page no call touched shows activity")
	}
}

// A clear whose timer fired as a call entered, too late for the call to stop
// it, finds the call and leaves the activity shown.
func TestAgentActivityStaleClearKeepsItShown(t *testing.T) {
	m, _, access, firstID, _ := newPaneHostManager(t)
	m.agentLinger = time.Hour
	p := livePage(t, m, access, firstID)
	ctx, end := withAgentCall(t.Context())
	if _, err := m.Open(ctx, access, "https://example.test/a", OpenOptions{PageID: firstID}); err != nil {
		t.Fatal(err)
	}
	end()
	p.activity.mu.Lock()
	stale := p.activity.clearGen
	p.activity.mu.Unlock()
	ctx, end = withAgentCall(t.Context())
	defer end()
	if _, err := m.Open(ctx, access, "https://example.test/b", OpenOptions{PageID: firstID}); err != nil {
		t.Fatal(err)
	}
	m.clearAgentActivity(p, stale)
	if !agentShown(m, access, firstID) {
		t.Fatal("a stale clear hid the activity of a call in the page")
	}
}

// A call through the MCP server is an agent's call.
func TestMCPToolCallShowsOnThePageTab(t *testing.T) {
	m, _, access, firstID, _ := newPaneHostManager(t)
	m.agentLinger = time.Hour
	server := NewMCPServer(m, true)
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread(access, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	url := config[ServerName].(map[string]any)["url"].(string)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "browser_open", "arguments": map[string]any{"url": "https://example.test/tool", "page_id": firstID},
	}})
	if response["error"] != nil {
		t.Fatalf("response = %#v", response)
	}
	if !agentShown(m, access, firstID) {
		t.Fatal("the tool call did not show on its page's tab")
	}
}
