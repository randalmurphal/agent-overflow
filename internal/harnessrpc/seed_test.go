package harnessrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness"
	"agent-overflow/internal/store"
)

func TestHarnessSeedRefusesTraversalProjectNames(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	for _, name := range []string{"../outside", "a/b", `a\b`, ".", ".."} {
		_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
			Name: name,
			Repo: &harness.RepoSpec{},
		}}})
		if err == nil || !strings.Contains(err.Error(), "plain directory name") {
			t.Fatalf("HarnessSeed(name=%q): err = %v, want plain-directory-name refusal", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the workspaces root (stat err %v)", err)
	}
}

func TestHarnessSeedProviderHomeFilesWriteAndResetWipes(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	home := filepath.Join(receiver.config.DataRoot, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	receiver.config.CredentialHome = home
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{
		{Path: ".claude.json", Content: `{"mcpServers":{}}`},
		{Path: ".claude/projects/-tmp-x/abc.jsonl", Content: `{"type":"summary"}`},
	}})
	if err != nil {
		t.Fatalf("seed providerHome: %v", err)
	}
	if len(result.HomeFiles) != 2 {
		t.Fatalf("HomeFiles = %v, want 2 entries", result.HomeFiles)
	}
	for _, rel := range []string{".claude.json", ".claude/projects/-tmp-x/abc.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("seeded home file %s: %v", rel, err)
		}
	}

	for _, bad := range []string{"", ".", "..", "../outside", "/etc/passwd", `C:\evil`, ".claude/../../out"} {
		if _, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{{Path: bad, Content: "x"}}}); err == nil {
			t.Fatalf("seed providerHome path %q: no error, want traversal refusal", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "out")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the home root (stat err %v)", err)
	}

	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	for _, rel := range []string{".claude.json", ".claude"} {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Fatalf("reset left provider home state %s (stat err %v)", rel, err)
		}
	}
	if _, err := os.Stat(gitconfig); err != nil {
		t.Fatalf("reset removed .gitconfig: %v", err)
	}
}

// Reset pauses the merged pull request cleanup before it deletes anything
// and resumes it after the cascade, so no pass begun for one test reads the
// forge or acts during the next.
func TestHarnessResetPausesTheMergedPRCleanupAroundTheCascade(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	if _, err := host.CreateProject(filepath.Join(t.TempDir(), "ws")); err != nil {
		t.Fatal(err)
	}
	projects := func() int {
		list, err := host.store.ListProjectsWithThreadCounts()
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	atPause, atResume := -1, -1
	host.onPause = func() { atPause = projects() }
	host.onResume = func() { atResume = projects() }
	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	if host.mergedPRPauses != 1 || host.mergedPRResumes != 1 {
		t.Fatalf("pauses = %d, resumes = %d, want 1 each", host.mergedPRPauses, host.mergedPRResumes)
	}
	if atPause != 1 || atResume != 0 {
		t.Fatalf("projects at pause = %d, at resume = %d; want the pause before the cascade and the resume after", atPause, atResume)
	}
}

func TestHarnessSeedWorkflowTargetValidationNamesTarget(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	_, err := receiver.seedWorkflowItems("project", HarnessSeedWorkflowItem{
		Workflow: "flow", Goal: "goal", Target: "queued",
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported target "queued"`) {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestHarnessSeedWorkflowCountIsBoundedBeforeMutation(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "too-many",
		Repo: &harness.RepoSpec{},
		Workflows: &HarnessSeedWorkflows{Items: []HarnessSeedWorkflowItem{{
			Workflow: "flow", Goal: "goal", Count: maxHarnessSeedWorkflowItems + 1,
		}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "expanded item count exceeds") {
		t.Fatalf("oversized workflow seed error = %v", err)
	}
	projects, listErr := host.store.ListProjects()
	if listErr != nil || len(projects) != 0 {
		t.Fatalf("oversized seed mutated projects: %+v, %v", projects, listErr)
	}
}

// Seeded history goes in behind the store, so the row CreateThread announced
// (IsDraft=true, no items) must be re-broadcast once the items exist. A thread
// seeded without history has nothing new to say.
func TestHarnessSeedBroadcastsRowsWhoseHistoryLandedBehindTheStore(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "broadcast-after-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{
			{Title: "with history", Turns: []HarnessSeedTurn{{
				UserText: "hi",
				Items:    []HarnessSeedItem{{Kind: "assistant_text", Summary: "Done."}},
			}}},
			{Title: "with session ref", SessionRef: "session-1"},
			{Title: "empty"},
		},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := result.Projects[0].ThreadIDs
	want := []string{ids[0], ids[1]}
	if strings.Join(host.broadcastRows, ",") != strings.Join(want, ",") {
		t.Fatalf("broadcast rows = %v, want %v", host.broadcastRows, want)
	}
	row, err := host.store.GetThread(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if row.IsDraft {
		t.Fatal("seeded thread with history still reads as a draft; the broadcast would re-announce a draft row")
	}
}

// A seeded assistant is an assistant's current thread whose row is
// announced with its record; a thread spawned by it names it.
func TestHarnessSeedMakesAnAssistant(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "assistant-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{
			{Title: "Ops", Assistant: true},
			{Title: "Worker", SpawnedBy: "Ops"},
		},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := result.Projects[0].ThreadIDs
	if agent, found, err := host.store.GetStandingAgent(ids[0]); err != nil || !found || agent.ThreadID != ids[0] {
		t.Fatalf("assistant record = %+v %v %v", agent, found, err)
	}
	if !slices.Contains(host.broadcastRows, ids[0]) {
		t.Fatalf("broadcast rows = %v, want the assistant's row announced", host.broadcastRows)
	}
	worker, err := host.store.GetThread(ids[1])
	if err != nil || worker.SpawnedByThreadID != ids[0] || !worker.SpawnedByAgent || worker.Placement != store.PlacementNested {
		t.Fatalf("worker = spawnedBy %q agent %v placement %s (%v)", worker.SpawnedByThreadID, worker.SpawnedByAgent, worker.Placement, err)
	}
}

// A seeded assistant lives outside every project, so the project cascade
// leaves it; the reset deletes it and every other thread outside a
// project, archived ones included.
func TestHarnessResetDeletesThreadsOutsideProjects(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name:    "assistant-reset",
		Repo:    &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Title: "Ops", Assistant: true}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ops := result.Projects[0].ThreadIDs[0]
	now := time.Now().UnixMilli()
	if err := host.store.CreateThread(store.Thread{ID: "loose", Title: "Loose", Provider: "claude", WorkspacePath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.store.ArchiveThread("loose"); err != nil {
		t.Fatal(err)
	}
	if outside, err := host.store.ThreadIDsOutsideProjects(); err != nil || len(outside) != 2 || outside[0] != ops {
		t.Fatalf("before the reset: %v, %v; want the assistant first of two", outside, err)
	}
	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if outside, err := host.store.ThreadIDsOutsideProjects(); err != nil || len(outside) != 0 {
		t.Fatalf("after the reset: %v, %v", outside, err)
	}
	if _, found, err := host.store.GetStandingAgent(ops); err != nil || found {
		t.Fatalf("the assistant survived the reset: %v %v", found, err)
	}
}

// The reset removes the user and project memory scopes, which no thread
// owns.
func TestHarnessResetRemovesTheMemoryScopes(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	fact := filepath.Join(receiver.config.DataDir, "memory", "user", "a.md")
	if err := os.MkdirAll(filepath.Dir(fact), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fact, []byte("---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(fact))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the memory scopes survived the reset: %v", err)
	}
}

// A seeded turn can open with a message another agent wrote, carrying the
// origin meta a real delivery writes on the turn's user row.
func TestHarnessSeedWritesTheUserMessageMeta(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	meta := `{"origin":"agent-thread","originThread":{"threadId":"t-lead","kind":"send"}}`
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "user-meta",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Turns: []HarnessSeedTurn{
			{UserText: "mine"},
			{UserText: "from the lead", UserMeta: json.RawMessage(meta)},
		}}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	items, err := host.store.ListItems(result.Projects[0].ThreadIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Meta != "{}" || items[1].Meta != meta {
		t.Fatalf("user rows = %+v, want an empty meta then %s", items, meta)
	}
}

// TestHarnessSeedRepeatsTurnsAndPadsPayloads covers the two primitives a
// windowing or paging fixture needs: a thread far larger than a literal
// fixture can describe, and one payload far larger than a fixture can
// ship. Both write through the store's ordinary item path.
func TestHarnessSeedRepeatsTurnsAndPadsPayloads(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "bulk-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{
			Title: "Long thread",
			Turns: []HarnessSeedTurn{
				{
					UserText: "keep sweeping",
					Items: []HarnessSeedItem{
						{Kind: "assistant_text", Summary: "swept"},
						{Kind: "assistant_text", Summary: "swept again"},
					},
					Repeat: 50,
				},
				{
					UserText: "show the log",
					Items: []HarnessSeedItem{{
						Kind:     "tool_call",
						ToolName: "Bash",
						Summary:  "go test ./...",
						Payload: &HarnessSeedPayload{
							Kind:      "tool_call_result",
							Data:      "NEEDLE the tokenizer overran the escape\n",
							PadBefore: 4096,
							PadAfter:  8192,
						},
					}},
				},
			},
		}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	threadID := result.Projects[0].ThreadIDs[0]

	items, err := host.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	// 50 repeats of (user + 2 items), then the one tool turn's user row
	// and its tool row.
	if len(items) != 50*3+2 {
		t.Fatalf("seeded %d items, want %d", len(items), 50*3+2)
	}
	// Every repeat is its own turn at its own index, in order.
	for i, item := range items[:150] {
		if item.TurnIndex != i/3 {
			t.Fatalf("item %d has turn index %d, want %d", i, item.TurnIndex, i/3)
		}
	}
	tool := items[len(items)-1]
	if tool.PayloadID == "" {
		t.Fatal("the tool row carries no payload")
	}

	data, total, _, err := host.store.GetPayloadChunk(threadID, tool.PayloadID, 0, 1<<20)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	needle := "NEEDLE the tokenizer overran the escape\n"
	if total < 4096+len(needle)+8192 {
		t.Fatalf("payload is %d bytes, want at least the padding plus the data", total)
	}
	at := strings.Index(string(data), needle)
	if at < 4096 {
		t.Fatalf("data sits at offset %d, want it after the leading padding", at)
	}
	if strings.Count(string(data), needle) != 1 {
		t.Fatalf("data appears %d times, want once", strings.Count(string(data), needle))
	}
	if !strings.HasPrefix(string(data), "pad before 000001:") {
		t.Fatalf("payload starts with %q, want the generated leading padding", string(data[:32]))
	}
	if !strings.Contains(string(data[at+len(needle):]), "pad after  000001:") {
		t.Fatal("the payload has no trailing padding after the data")
	}
	// Every filler line is one width, so the data's offset is computable.
	if want := ((4096 + seedPadLineBytes - 1) / seedPadLineBytes) * seedPadLineBytes; at != want {
		t.Fatalf("data sits at offset %d, want %d (whole padding lines)", at, want)
	}
}

func TestHarnessSeedRefusesOversizedPayloadPadding(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "bad-padding",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Turns: []HarnessSeedTurn{{
			UserText: "hi",
			Items: []HarnessSeedItem{{
				Kind:    "tool_call",
				Summary: "Bash",
				Payload: &HarnessSeedPayload{Kind: "tool_call_result", Data: "x", PadAfter: seedPadLineLimit + 1},
			}},
		}}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("err = %v, want the padding limit refusal", err)
	}
}

// TestHarnessSeedsTheWindowedThreadFixture seeds the 38k-item thread
// thread-tools-paired.spec.ts windows, pages and exports, and reads it the
// way those tools do: its bounds, the anchor's search hit and row, the
// window of two turns around it, and every row in order.
func TestHarnessSeedsTheWindowedThreadFixture(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	const bulkTurns, bulkItems = 1000, 18
	bulk := HarnessSeedTurn{UserText: "keep sweeping", Repeat: bulkTurns}
	for i := range bulkItems {
		bulk.Items = append(bulk.Items, HarnessSeedItem{Kind: "assistant_text", Summary: fmt.Sprintf("swept region %d", i+1)})
	}
	single := func(user, summary string) HarnessSeedTurn {
		return HarnessSeedTurn{UserText: user, Items: []HarnessSeedItem{{Kind: "assistant_text", Summary: summary}}}
	}
	start := time.Now()
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "remote-window-target",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Title: "Remote kernel sweep", Turns: []HarnessSeedTurn{
			single("begin the sweep", "PAIRBIGHEAD sweep started"),
			bulk,
			single("what went wrong in the middle", "PAIRBIGANOMALY the scheduler stalled at tick 41"),
			bulk,
			single("wrap up", "PAIRBIGTAIL sweep complete"),
		}}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Logf("seeded in %s", time.Since(start))
	threadID := result.Projects[0].ThreadIDs[0]
	database := host.store
	const turns, items = 3 + 2*bulkTurns, 3*2 + 2*bulkTurns*(bulkItems+1)

	thread, err := database.GetThread(threadID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	if thread.IsDraft {
		t.Fatal("the seeded thread reads as a draft")
	}
	stamp, found, err := database.ThreadHistoryStamp(threadID)
	if err != nil || !found {
		t.Fatalf("read history stamp: found=%v err=%v", found, err)
	}
	if stamp.Rev != items {
		t.Fatalf("history_rev %d, want one revision per item (%d)", stamp.Rev, items)
	}
	settled, err := database.ListRecentTurns(threadID, turns+1)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(settled) != turns {
		t.Fatalf("seeded %d turns, want %d", len(settled), turns)
	}
	for _, turn := range settled {
		if turn.CompletedAt == nil || turn.StopReason != "end_turn" {
			t.Fatalf("turn %d is not settled: %+v", turn.TurnIndex, turn)
		}
	}

	first, last, ok, err := database.ThreadTimelineBounds(threadID)
	if err != nil || !ok {
		t.Fatalf("timeline bounds: ok=%v err=%v", ok, err)
	}
	if first.TurnIndex != 0 || first.ItemIndex != 0 || last.TurnIndex != turns-1 || last.ItemIndex != 1 {
		t.Fatalf("timeline runs %+v..%+v, want turn 0 item 0 to turn %d item 1", first, last, turns-1)
	}

	hits, err := database.SearchThreads("PAIRBIGANOMALY", store.ThreadSearchFilter{ThreadIDs: []string{threadID}, Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("search hits = %+v, want the anchor row alone", hits)
	}
	anchor, found, err := database.GetThreadItem(threadID, hits[0].ItemID)
	if err != nil || !found {
		t.Fatalf("read anchor %s: found=%v err=%v", hits[0].ItemID, found, err)
	}
	if anchor.TurnIndex != 1+bulkTurns || anchor.ItemIndex != 1 {
		t.Fatalf("anchor at turn %d item %d, want turn %d item 1", anchor.TurnIndex, anchor.ItemIndex, 1+bulkTurns)
	}
	around, err := database.ListItemsInRange(threadID,
		store.TimelineCursor{TurnIndex: anchor.TurnIndex - 2, ItemIndex: math.MinInt32},
		store.TimelineCursor{TurnIndex: anchor.TurnIndex + 2, ItemIndex: math.MaxInt32}, 1000, false)
	if err != nil {
		t.Fatalf("read around the anchor: %v", err)
	}
	if want := 2 + 4*(bulkItems+1); len(around) != want {
		t.Fatalf("two turns around the anchor hold %d rows, want %d", len(around), want)
	}

	all, err := database.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(all) != items {
		t.Fatalf("seeded %d items, want %d", len(all), items)
	}
	ids := make(map[string]bool, len(all))
	for i, item := range all {
		if i > 0 {
			prev := all[i-1]
			next := item.TurnIndex == prev.TurnIndex+1 && item.ItemIndex == 0
			if !next && (item.TurnIndex != prev.TurnIndex || item.ItemIndex != prev.ItemIndex+1) {
				t.Fatalf("row %d at turn %d item %d follows turn %d item %d", i, item.TurnIndex, item.ItemIndex, prev.TurnIndex, prev.ItemIndex)
			}
		}
		if ids[item.ID] {
			t.Fatalf("item id %s repeats", item.ID)
		}
		ids[item.ID] = true
	}
	if all[1].Summary != "PAIRBIGHEAD sweep started" || all[len(all)-1].Summary != "PAIRBIGTAIL sweep complete" {
		t.Fatalf("thread runs %q..%q, want the head and tail rows", all[1].Summary, all[len(all)-1].Summary)
	}
}

// A seeded thread can name the seeded thread that spawned it: the row then
// names its spawner the way a real thread_spawn's does, and it is broadcast
// again once it does.
func TestHarnessSeedRecordsASpawner(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "spawned-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{
			{Title: "Agent"},
			{Title: "Child", SpawnedBy: "Agent"},
		},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := result.Projects[0].ThreadIDs
	row, err := host.store.GetThread(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if row.SpawnedByThreadID != ids[0] {
		t.Fatalf("child names spawner %q, want %s", row.SpawnedByThreadID, ids[0])
	}
	if strings.Join(host.broadcastRows, ",") != ids[1] {
		t.Fatalf("broadcast rows = %v, want the child alone", host.broadcastRows)
	}
	_, err = Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name:    "unknown-spawner",
		Repo:    &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Title: "Child", SpawnedBy: "Nobody"}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "names no thread seeded earlier") {
		t.Fatalf("seed with an unknown spawner: %v", err)
	}
}
