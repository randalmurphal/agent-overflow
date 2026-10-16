package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// agentTreeShape is a standing agent's tree at the size the Agents segment
// is read at: one chief that spawned three managers, which spawned ten
// workers, with the chief owed 200 open sends and holding 300 settled
// answers of 4 KiB each, every one confirmed in a wake row of its own.
type agentTreeShape struct {
	open, settled []string
}

const (
	agentTreeManagers = 3
	agentTreeWorkers  = 10
	agentTreeOpen     = 200
	agentTreeSettled  = 300
	agentTreeAnswer   = 4 << 10
)

func seedAgentTree(t *testing.T, f *requestFixture) agentTreeShape {
	t.Helper()
	chief := f.caller.ID
	now := time.Now().UnixMilli()
	created := now - int64(agentTreeOpen+agentTreeSettled+agentTreeManagers+agentTreeWorkers) - 1000
	insert := func(r store.ThreadRequest) {
		t.Helper()
		created++
		r.CreatedAt = created
		if err := f.app.store.InsertThreadRequest(r); err != nil {
			t.Fatalf("insert %s: %v", r.Token, err)
		}
	}
	thread := func() string {
		t.Helper()
		th, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
		if err != nil {
			t.Fatalf("create thread: %v", err)
		}
		return th.ID
	}
	managers := make([]string, agentTreeManagers)
	for i := range managers {
		managers[i] = thread()
		insert(store.ThreadRequest{Token: fmt.Sprintf("spawn-m%d", i), CallerThreadID: chief, Kind: store.ThreadRequestSpawn,
			TargetThreadID: managers[i], Notify: true, State: store.ThreadRequestRunning})
	}
	workers := make([]string, agentTreeWorkers)
	for i := range workers {
		workers[i] = thread()
		insert(store.ThreadRequest{Token: fmt.Sprintf("spawn-w%d", i), CallerThreadID: managers[i%agentTreeManagers],
			Kind: store.ThreadRequestSpawn, TargetThreadID: workers[i], Notify: true, State: store.ThreadRequestRunning})
	}
	var shape agentTreeShape
	answer := []byte(strings.Repeat("answer text ", agentTreeAnswer/12+1)[:agentTreeAnswer])
	// Settled and open sends interleave, so a listing bounded at its row
	// limit reads both kinds.
	for i := 0; i < agentTreeOpen+agentTreeSettled; i++ {
		target := workers[i%agentTreeWorkers]
		if i%5 < 2 && len(shape.open) < agentTreeOpen || len(shape.settled) == agentTreeSettled {
			token := fmt.Sprintf("open-%03d", len(shape.open))
			insert(store.ThreadRequest{Token: token, CallerThreadID: chief, Kind: store.ThreadRequestSend,
				TargetThreadID: target, Notify: true, State: store.ThreadRequestRunning})
			shape.open = append(shape.open, token)
			continue
		}
		token := fmt.Sprintf("settled-%03d", len(shape.settled))
		insert(store.ThreadRequest{Token: token, CallerThreadID: chief, Kind: store.ThreadRequestSend,
			TargetThreadID: target, Notify: true, State: store.ThreadRequestRunning})
		if ok, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: answer, AnswerKind: store.ThreadAnswerFinal, SettledAt: now,
		}); err != nil || !ok {
			t.Fatalf("settle %s: %v %v", token, ok, err)
		}
		meta, err := json.Marshal(map[string]any{"sendId": threadWakeSendID(token), "origin": "agent-wake"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.store.UpsertItem(store.Item{
			ID: "wake-" + token, ThreadID: chief, TurnIndex: len(shape.settled) + 1, Kind: "user_text", Role: "user",
			Status: "completed", Summary: "the answer", Meta: string(meta), CreatedAt: now, UpdatedAt: now,
		}, nil); err != nil {
			t.Fatalf("write the wake row of %s: %v", token, err)
		}
		if ok, err := f.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeQueued, now, false); err != nil || !ok {
			t.Fatalf("mark %s delivered: %v %v", token, ok, err)
		}
		f.app.confirmOwnedDeliveries(chief, []triage.ConsumedSend{{ItemID: "wake-" + token, SendIDs: []string{threadWakeSendID(token)}}})
		shape.settled = append(shape.settled, token)
	}
	waitAgentEmitsIdle(t, f.app)
	return shape
}

// waitAgentEmitsIdle waits out the frames the seeding queued.
func waitAgentEmitsIdle(t *testing.T, a *App) {
	t.Helper()
	waitUntil(t, 30*time.Second, func() bool {
		e := &a.threadAgentEmits
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.draining
	})
}

// agentTreeCost is what one leg costs per call: the fewest read statements
// any run issued, so a background read landing in one run cannot count
// against the leg, and the mean time.
type agentTreeCost struct {
	reads float64
	per   time.Duration
}

func measureAgentTree(a *App, runs int, call func()) agentTreeCost {
	call()
	var spent time.Duration
	fewest := uint64(1 << 62)
	for range runs {
		start, reads := time.Now(), a.store.ReadCount()
		call()
		spent += time.Since(start)
		fewest = min(fewest, a.store.ReadCount()-reads)
	}
	return agentTreeCost{reads: float64(fewest), per: spent / time.Duration(runs)}
}

// TestAgentTreeReadCost pins the Agents segment's reads over the tree above
// to a number of statements that does not grow with it: the listing (the
// thread, the tree's shape, its rows, their targets' names and last
// messages), a pane's read of up to 32 rows at two levels (the listing's
// statements over the named rows, so a pane reads named rows up to that
// bound and the list past it), a tool rows'
// state read of 256 settled tokens, the change frames of the 200 open sends,
// and an assistant's session check on a Claude resume. Run it alone for its
// timings.
func TestAgentTreeReadCost(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	shape := seedAgentTree(t, f)
	chief := f.caller.ID

	list := measureAgentTree(f.app, 20, func() {
		listing, err := f.app.ListThreadAgentRows(t.Context(), chief)
		if err != nil || len(listing.Rows) != threadAgentRowLimit || !listing.Hidden {
			t.Fatalf("ListThreadAgentRows: %d rows, hidden %v, %v", len(listing.Rows), listing.Hidden, err)
		}
		for _, row := range listing.Rows {
			if strings.HasPrefix(row.Token, "settled-") && (row.WakeItemID != "wake-"+row.Token || row.Answer == "") {
				t.Fatalf("settled row %+v; want its answer and wake row", row)
			}
		}
	})
	tree := measureAgentTree(f.app, 20, func() {
		if _, _, err := newThreadAgentView(f.app).tree(chief, threadAgentRowDepth, nil, threadAgentRowLimit); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("tree read alone: %.1f reads, %v per call", tree.reads, tree.per)
	// Row reads of the chief's open sends and the managers' spawns, at one
	// and two levels.
	named := append(slices.Clone(shape.open), "spawn-w0", "spawn-w1", "spawn-w2")
	rowReads := map[int]agentTreeCost{}
	for _, n := range []int{1, 8, maxThreadAgentRowReads} {
		tokens := append(slices.Clone(named[:n-min(n, 3)]), named[len(named)-min(n, 3):]...)
		rowReads[n] = measureAgentTree(f.app, 20, func() {
			reads, err := f.app.GetThreadAgentRows(chief, tokens)
			if err != nil || len(reads) != n {
				t.Fatalf("GetThreadAgentRows: %d reads, %v", len(reads), err)
			}
			for _, read := range reads {
				if !read.Listed {
					t.Fatalf("%s is not listed", read.Token)
				}
			}
		})
		t.Logf("row read of %d: %.1f reads, %v per call", n, rowReads[n].reads, rowReads[n].per)
	}
	states := measureAgentTree(f.app, 20, func() {
		got, err := f.app.GetThreadRequestStates(chief, shape.settled[:maxThreadRequestStates])
		if err != nil || len(got) != maxThreadRequestStates {
			t.Fatalf("GetThreadRequestStates: %d states, %v", len(got), err)
		}
		for _, state := range got {
			if !state.Answered || state.WakeItemID != "wake-"+state.Token {
				t.Fatalf("state %+v; want answered in its wake row", state)
			}
		}
	})
	frames := measureAgentTree(f.app, 20, func() {
		e := &f.app.threadAgentEmits
		e.mu.Lock()
		for _, token := range shape.open {
			e.tokens[token] = struct{}{}
		}
		e.draining = true
		e.mu.Unlock()
		f.app.drainThreadAgentEmits()
	})
	// An assistant's thread whose transcript is gone: the check reads the
	// capability, looks for the file and finds none.
	assistant := f.plainThread(t)
	testAssistant(t, f.app, assistant.ID)
	assistant, err := f.app.store.GetThread(assistant.ID)
	if err != nil || assistant.AgentID == "" {
		t.Fatalf("the assistant's thread reads agent %q, %v", assistant.AgentID, err)
	}
	workDir := t.TempDir()
	resume := measureAgentTree(f.app, 20, func() {
		if err := f.app.requireAssistantSession(assistant, provider.SessionOptions{Resume: "session-1", WorkDir: workDir}); !errors.Is(err, errAssistantSessionGone) {
			t.Fatalf("requireAssistantSession = %v; want the session gone", err)
		}
	})
	t.Logf("assistant session check on a Claude resume: %.1f reads, %v per start", resume.reads, resume.per)
	t.Logf("listing: %.1f reads, %v per call", list.reads, list.per)
	t.Logf("request states (256 tokens): %.1f reads, %v per call", states.reads, states.per)
	t.Logf("frames (200 tokens): %.1f reads, %v per drain", frames.reads, frames.per)
	for _, leg := range []struct {
		name        string
		reads, most float64
	}{
		{"the listing", list.reads, 5}, {"the tree", tree.reads, 2}, {"the request states", states.reads, 1},
		{"a drain of frames", frames.reads, 1}, {"the session check", resume.reads, 0},
		{"a row read of 1", rowReads[1].reads, 4}, {"a row read of 8", rowReads[8].reads, 4},
		{"a row read of 32", rowReads[maxThreadAgentRowReads].reads, 4},
	} {
		if leg.reads > leg.most {
			t.Errorf("%s ran %.1f reads; want at most %.0f", leg.name, leg.reads, leg.most)
		}
	}
}
