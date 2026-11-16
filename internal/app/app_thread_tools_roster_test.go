package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// ownSpawn makes worker an owned spawn of caller, as a local thread_spawn's
// naming write does.
func ownSpawn(t *testing.T, app *App, token, caller, worker string) {
	t.Helper()
	if _, err := app.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
	}, store.ThreadRequestReceipt{Token: token, OwnerDeviceID: threadReceiptLocalOwner, Kind: store.ThreadRequestSpawn, SourceThreadID: caller}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.NameLocalThreadRequestTarget(token, worker, true); err != nil {
		t.Fatal(err)
	}
}

// subThreadChain makes a thread the person talks to and n owned workers under
// it, one below the other, and returns them from the top.
func subThreadChain(t *testing.T, app *App, providerName, model string, n int) []store.Thread {
	t.Helper()
	workspace := t.TempDir()
	chain := make([]store.Thread, 0, n+1)
	for i := 0; i <= n; i++ {
		thread, err := createTestThread(t, app, providerName, workspace, model, "chat")
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			ownSpawn(t, app, "spawn-"+thread.ID, chain[i-1].ID, thread.ID)
		}
		chain = append(chain, thread)
	}
	return chain
}

func toolNames(tools []map[string]any) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool["name"].(string))
	}
	return names
}

// A session's tool list at each level: thread_spawn up to the level above
// the limit, none at it, and every other tool at every level. A worker
// placed in the sidebar is level 0 again.
func TestThreadToolsAtEachSubThreadLevel(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{"subThreadLevels": 2}); err != nil {
		t.Fatal(err)
	}
	chain := subThreadChain(t, app, string(provider.Claude), "claude-opus-4-7", 2)
	for level, thread := range chain {
		place, err := app.subThreadPlaceOf(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if place.level != level || place.limit != 2 {
			t.Fatalf("level %d: place = %+v", level, place)
		}
		names := toolNames(app.threadMCPTools(threadMCPAccess{ThreadID: thread.ID, Place: place}))
		if spawns := slices.Contains(names, "thread_spawn"); spawns != (level < 2) {
			t.Errorf("level %d: thread_spawn listed = %v", level, spawns)
		}
		for _, kept := range []string{"thread_send", "thread_ask", "thread_reply", "thread_status"} {
			if !slices.Contains(names, kept) {
				t.Errorf("level %d: %s is missing", level, kept)
			}
		}
	}
	if _, err := app.store.SetThreadPlacement([]string{chain[1].ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if place, err := app.subThreadPlaceOf(chain[2].ID); err != nil || place.level != 1 || !place.canSpawn() {
		t.Fatalf("under a worker the person placed in the sidebar: place = %+v, %v", place, err)
	}
}

// A session at the limit loses its provider's own subagents: Claude's
// launcher under both its names on Claude and Claude's terminal, and
// Codex's collaboration agents. A session below it keeps them.
func TestASessionAtTheLimitLosesItsNativeSubagents(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		string(provider.Claude):    {"Agent", "Task"},
		string(provider.ClaudeTUI): {"Agent", "Task"},
		string(provider.Codex):     {codex.ToggleCollabAgents},
	}
	for providerName, want := range cases {
		opts := provider.SessionOptions{DisabledTools: []string{"WebSearch"}}
		withoutNativeSubagents(&opts, providerName, subThreadPlace{level: 1, limit: 2})
		if !slices.Equal(opts.DisabledTools, []string{"WebSearch"}) {
			t.Errorf("%s below the limit: disabled = %v", providerName, opts.DisabledTools)
		}
		withoutNativeSubagents(&opts, providerName, subThreadPlace{level: 2, limit: 2})
		if !slices.Equal(opts.DisabledTools, append([]string{"WebSearch"}, want...)) {
			t.Errorf("%s at the limit: disabled = %v, want %v added", providerName, opts.DisabledTools, want)
		}
	}
	if overrides := codex.DisabledToolConfigOverrides([]string{codex.ToggleCollabAgents}); overrides["agents.enabled"] != false {
		t.Fatalf("the Codex toggle does not turn collaboration agents off: %v", overrides)
	}
}

// End to end on Claude: a worker at the limit starts with Agent and Task
// disallowed and a thread tools registration with no thread_spawn.
func TestStartSession_AWorkerAtTheLimitStartsWithoutSpawning(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	if _, err := app.settings.Update(map[string]any{"subThreadLevels": 1}); err != nil {
		t.Fatal(err)
	}
	chain := subThreadChain(t, app, string(provider.Claude), "claude-opus-4-7", 1)
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
		t.Fatal(err)
	}
	if err := app.StartSession(chain[1].ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	for _, tool := range []string{"Agent", "Task"} {
		if !hasArgPair(argv, "--disallowedTools", tool) {
			t.Errorf("argv does not disallow %s: %v", tool, argv)
		}
	}
	appendPath, _ := argValue(argv, "--append-system-prompt-file")
	if content, err := os.ReadFile(appendPath); err != nil || strings.Contains(string(content), "`thread_spawn`") {
		t.Errorf("append file at the limit: %q, %v", content, err)
	}
	// The tool list the session itself reads, over the registration its
	// start made.
	raw, _ := argValue(argv, "--mcp-config")
	var config struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config.MCPServers[threadMCPName].URL == "" {
		t.Fatalf("--mcp-config = %q, %v", raw, err)
	}
	status, reply := remoteMCPRequest(t, config.MCPServers[threadMCPName].URL, "tools/list", map[string]any{})
	var listing struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(reply["result"], &listing); err != nil || status != 200 {
		t.Fatalf("tools/list: %d %s %v", status, reply, err)
	}
	names := toolNames(listing.Tools)
	if slices.Contains(names, "thread_spawn") || !slices.Contains(names, "thread_send") {
		t.Errorf("the worker at the limit lists %v", names)
	}
}

// A spawn that still reaches the app from a thread at the limit, from a
// session that started before the limit applied, is refused naming it.
func TestALateSpawnFromAThreadAtTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	if _, err := f.app.settings.Update(map[string]any{"subThreadLevels": 1}); err != nil {
		t.Fatal(err)
	}
	lead := f.thread(t, "lead")
	worker := f.thread(t, "worker")
	ownSpawn(t, f.app, "tok", lead.ID, worker.ID)
	_, err := f.adapter.Spawn(t.Context(), threadtools.Caller{ThreadID: worker.ID}, threadtools.SpawnCall{Prompt: "more"})
	code, message, public := errorsx.PublicDetails(err)
	if !public || code != threadtools.CodeInvalidRequest || !strings.Contains(message, "This thread is 1 level of sub-threads below") || !strings.Contains(message, "the limit is 1") {
		t.Fatalf("late spawn = %v (%q, %q)", err, code, message)
	}
}

// What a session appends keeps one order on every provider: the app guide,
// a standing agent's block, on Codex the server guides, and last the
// spawn's instructions, which replace nothing.
func TestSpawnInstructionsAreAppendedLastOnEveryProvider(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	registered := map[string]any{threadMCPName: map[string]any{}}
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		thread := store.Thread{ID: "t-" + providerName, Provider: providerName, Mode: "chat", SpawnInstructions: "ROLE"}
		prompts, err := app.sessionPromptsFor(thread, registered, "BLOCK", subThreadPlace{})
		if err != nil {
			t.Fatal(err)
		}
		guide := agentGuideText(app.agentGuideServersOn(thread.ID, registered))
		if want := guide + "\n\nBLOCK\n\nROLE"; prompts.claudeAppend != want {
			t.Errorf("%s: append = %q, want the guide, the block, then the instructions", providerName, prompts.claudeAppend)
		}
		if providerName != string(provider.Codex) {
			continue
		}
		if !strings.HasPrefix(prompts.codexDeveloper, guide+"\n\nBLOCK\n\n") || !strings.HasSuffix(prompts.codexDeveloper, "\n\nROLE") ||
			!strings.Contains(prompts.codexDeveloper, "Starting work.") {
			t.Errorf("codex developer instructions = %q", prompts.codexDeveloper)
		}
	}
	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": false}); err != nil {
		t.Fatal(err)
	}
	prompts, err := app.sessionPromptsFor(store.Thread{ID: "off", Provider: string(provider.Claude), Mode: "chat", SpawnInstructions: "ROLE"}, nil, "", subThreadPlace{})
	if err != nil || prompts.claudeAppend != "ROLE" {
		t.Fatalf("guide off: append = %q, %v; want the instructions alone", prompts.claudeAppend, err)
	}
}

// The instructions live on the thread, so every start reads them: a
// restart of the session carries them as the first start did.
func TestStartSession_SpawnInstructionsSurviveARestart(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := app.threadTools().createSpawnedThread(context.Background(), threadtools.SpawnCall{}, spawnOptions{CreateThreadOptions: CreateThreadOptions{
		ProjectID: mustProject(t, app).ID, Provider: string(provider.Claude), Model: "claude-opus-4-7", spawnInstructions: "Review for races.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
		t.Fatal(err)
	}
	for start := range 2 {
		if err := app.StartSession(thread.ID); err != nil {
			t.Fatalf("start %d: %v", start, err)
		}
		argv := spawnedClaudeArgv(t, argvPath)
		appendPath, _ := argValue(argv, "--append-system-prompt-file")
		content, err := os.ReadFile(appendPath)
		if err != nil || !strings.HasSuffix(string(content), "\n\nReview for races.") {
			t.Fatalf("start %d: append file = %q, %v", start, content, err)
		}
		if err := app.StopSession(thread.ID); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(argvPath); err != nil {
			t.Fatal(err)
		}
	}
}

func mustProject(t *testing.T, app *App) store.Project {
	t.Helper()
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// A provider the person turned off, or whose binary is not on this
// computer, is not offered: thread_options leaves it out, so no profile on
// it is listed and the guide names none.
func TestTheCatalogOffersOnlyInstalledEnabledProviders(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	claudeBinary := createMockBinary(t, "2.1.284 (Claude Code)")
	codexBinary := createMockBinary(t, "codex-cli 0.160.0")
	providers := func() []string {
		catalog, err := f.adapter.Catalog(t.Context(), threadtools.CatalogQuery{What: threadtools.OptionsModels})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, option := range catalog.Providers {
			ids = append(ids, option.ID)
		}
		return ids
	}
	cases := []struct {
		name  string
		patch map[string]any
		want  []string
	}{
		{"both", map[string]any{"claudeBinaryPath": claudeBinary, "codexBinaryPath": codexBinary}, []string{"claude", "codex"}},
		{"claude only installed", map[string]any{"codexBinaryPath": filepath.Join(t.TempDir(), "no-codex")}, []string{"claude"}},
		{"codex only installed", map[string]any{"claudeBinaryPath": filepath.Join(t.TempDir(), "no-claude"), "codexBinaryPath": codexBinary}, []string{"codex"}},
		{"codex turned off", map[string]any{"claudeBinaryPath": claudeBinary, "codexEnabled": false}, []string{"claude"}},
	}
	for _, tc := range cases {
		if _, err := f.app.settings.Update(tc.patch); err != nil {
			t.Fatal(err)
		}
		if got := providers(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: providers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A paired computer's spawn carries its thread's level: the destination
// stores it with the thread, counts that thread's own workers from it,
// and refuses a spawn whose thread would sit past its own limit. A spawn's
// instructions travel with it and are stored there.
func TestARemoteSpawnCarriesItsLevelAndInstructions(t *testing.T) {
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	installMockClaudeTurns(t, pair.source, [][]string{{mockClaudeInitLine}})
	lead, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	ownSpawn(t, pair.source, "lead-spawn", lead.ID, pair.caller.ID)

	ack := pair.spawnThere(t, "work over there", map[string]any{"instructions": "Answer in one line."})
	child := pair.receipt(t, ack.Token).TargetThreadID
	thread, err := pair.dest.store.GetThread(child)
	if err != nil {
		t.Fatal(err)
	}
	if thread.SpawnLevel != 2 || thread.SpawnInstructions != "Answer in one line." {
		t.Fatalf("the destination's thread carries level %d and instructions %q", thread.SpawnLevel, thread.SpawnInstructions)
	}
	if level, err := pair.dest.store.SubThreadLevel(child); err != nil || level != 2 {
		t.Fatalf("SubThreadLevel there = %d, %v", level, err)
	}

	standalone := pair.spawnThere(t, "for the user", map[string]any{"placement": threadtools.PlacementStandalone})
	if thread, err := pair.dest.store.GetThread(pair.receipt(t, standalone.Token).TargetThreadID); err != nil || thread.SpawnLevel != 0 {
		t.Fatalf("a spawn given a sidebar row carries level %d, %v; want 0", thread.SpawnLevel, err)
	}

	// An ask's copy there is one below the asker, as a local ask's is.
	consulted := forkableThreadOn(t, pair.dest, "consulted-there")
	ask, err := pair.adapter().Ask(context.Background(), pair.callerIdentity(), threadtools.AskCall{
		ThreadID: consulted.ID, ComputerID: pair.computer, Question: "what did you do?",
	})
	if err != nil {
		t.Fatalf("remote ask: %v", err)
	}
	copyID := pair.receipt(t, ask.Token).TargetThreadID
	if level, err := pair.dest.store.SubThreadLevel(copyID); err != nil || level != 2 {
		t.Fatalf("the ask copy there is at level %d, %v; want 2", level, err)
	}

	// The person filing the worker in a group there ends the delegation it
	// came with, and its next session can spawn again.
	group, err := pair.dest.store.CreateThreadGroup(thread.ProjectID, "Taken over")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pair.dest.SetThreadGroup([]string{child}, group.ID); err != nil {
		t.Fatal(err)
	}
	if place, err := pair.dest.subThreadPlaceOf(child); err != nil || place.level != 0 || !place.canSpawn() {
		t.Fatalf("the worker filed in a group there: %+v, %v", place, err)
	}

	if _, err := pair.dest.settings.Update(map[string]any{"subThreadLevels": 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{Prompt: "too deep", ComputerID: pair.computer, ProjectID: pair.project})
	if err == nil || !strings.Contains(err.Error(), "this computer allows 1") {
		t.Fatalf("a spawn past the destination's limit = %v", err)
	}
}
