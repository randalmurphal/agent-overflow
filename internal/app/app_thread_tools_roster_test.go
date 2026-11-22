package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
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
	thread, _, err := app.threadTools().createSpawnedThread(context.Background(), threadtools.SpawnCall{}, spawnOptions{CreateThreadOptions: CreateThreadOptions{
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

// For every seeded profile, three that inherit, one with an effort on a
// model that has none, and each caller and catalog below, what
// thread_options lists for a profile is what a spawn naming it runs with,
// through the tool calls an agent makes; a profile it does not list is
// refused.
func TestEveryListedProfileSpawnsWithWhatItLists(t *testing.T) {
	f := newRequestFixture(t)
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	// The Codex catalog the app ships, in place of the live one tests turn off.
	f.app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string, procutil.Reaper) ([]provider.ModelInfo, error) {
		return provider.CodexModels, nil
	}, time.Now)
	claudeBinary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{mockClaudeInitLine}})
	codexBinary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:   `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-thread"}}}`,
		`"method":"config/read"`:  `{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}`,
		`"method":"turn/start"`:   `{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1"}}}`,
	})
	profiles := append(slices.Clone(f.app.currentSettings().AgentProfiles),
		settings.AgentProfile{Name: "Like me", UseFor: "Anything."},
		settings.AgentProfile{Name: "Opus as me", UseFor: "Anything.", Provider: "claude", Model: "claude-opus-5"},
		settings.AgentProfile{Name: "Sol as me", UseFor: "Anything.", Provider: "codex", Model: "gpt-5.6-sol"},
		settings.AgentProfile{Name: "Effortless", UseFor: "Anything.", Provider: "claude", Model: "claude-haiku-4-5", Effort: "high"},
	)
	encoded, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	var roster []any
	if err := json.Unmarshal(encoded, &roster); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.settings.Update(map[string]any{
		"agentProfiles": roster, "claudeBinaryPath": claudeBinary, "codexBinaryPath": codexBinary,
	}); err != nil {
		t.Fatal(err)
	}

	callers := []struct{ provider, model, effort string }{
		{string(provider.Claude), "claude-opus-5", "medium"},
		{string(provider.Claude), "claude-sonnet-5", "max"},
		{string(provider.Codex), "gpt-5.6-sol", "xhigh"},
	}
	catalogs := []struct {
		name          string
		claude, codex bool
	}{{"both", true, true}, {"Claude only", true, false}, {"Codex only", false, true}}
	for _, catalog := range catalogs {
		if _, err := f.app.settings.Update(map[string]any{"claudeEnabled": catalog.claude, "codexEnabled": catalog.codex}); err != nil {
			t.Fatal(err)
		}
		for _, spec := range callers {
			caller, err := createTestThread(t, f.app, spec.provider, t.TempDir(), spec.model, threadmode.ModeChat)
			if err != nil {
				t.Fatal(err)
			}
			caller.ReasoningEffort = spec.effort
			if err := f.app.store.UpdateThread(caller); err != nil {
				t.Fatal(err)
			}
			token := uuid.NewString()
			f.app.sessionManager().put(caller.ID, session{Token: token, Provider: spec.provider})
			servers, err := f.app.threadMCPConfigForThread(caller, token, subThreadPlace{})
			if err != nil {
				t.Fatal(err)
			}
			endpoint := servers[threadMCPName].(map[string]any)["url"].(string)
			var options struct {
				Profiles []threadtools.ProfileOption `json:"profiles"`
			}
			if err := json.Unmarshal(remoteMCPCall(t, endpoint, "thread_options", map[string]any{}, false), &options); err != nil {
				t.Fatal(err)
			}
			where := catalog.name + ", caller " + spec.model + " at " + spec.effort
			if len(options.Profiles) == 0 {
				t.Fatalf("%s: thread_options lists no profiles", where)
			}
			t.Logf("%s: lists %v", where, threadtools.ProfileNames(options.Profiles))
			listed := map[string]threadtools.ProfileOption{}
			for _, option := range options.Profiles {
				listed[option.Name] = option
			}
			for _, profile := range profiles {
				option, offered := listed[profile.Name]
				if !offered {
					text := remoteMCPCall(t, endpoint, "thread_spawn", map[string]any{"prompt": "work", "profile": profile.Name}, true)
					if !strings.Contains(string(text), "cannot run") {
						t.Errorf("%s: %s is not listed, and its spawn answers %s", where, profile.Name, text)
					}
					continue
				}
				var ack struct {
					ThreadID string `json:"thread_id"`
				}
				if err := json.Unmarshal(remoteMCPCall(t, endpoint, "thread_spawn", map[string]any{"prompt": "work", "profile": profile.Name}, false), &ack); err != nil {
					t.Fatal(err)
				}
				spawned, err := f.app.store.GetThread(ack.ThreadID)
				if err != nil {
					t.Fatalf("%s: %s: %v", where, profile.Name, err)
				}
				if spawned.Provider != option.Provider || spawned.Model != option.Model || spawned.ReasoningEffort != option.Effort {
					t.Errorf("%s: %s lists %s/%s/%q and runs %s/%s/%q", where, profile.Name,
						option.Provider, option.Model, option.Effort, spawned.Provider, spawned.Model, spawned.ReasoningEffort)
				}
			}
		}
	}
}

// The roster editor's check is the spawn's resolution: a profile whose
// effort its model does not offer here is a problem the save is refused
// for, and one whose provider this computer lacks is only noted.
func TestTheEditorRefusesAProfileItsModelCannotRun(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": createMockBinary(t, "2.1.284 (Claude Code)"),
		"codexBinaryPath":  filepath.Join(t.TempDir(), "no-codex"),
	}); err != nil {
		t.Fatal(err)
	}
	profiles := []settings.AgentProfile{
		{Name: "Fine", UseFor: "x", Provider: "claude", Model: "claude-opus-5", Effort: "high"},
		{Name: "Effortless", UseFor: "x", Provider: "claude", Model: "claude-haiku-4-5", Effort: "high"},
		{Name: "Elsewhere", UseFor: "x", Provider: "codex", Model: "gpt-5.6-sol", Effort: "high"},
	}
	checks, err := app.CheckAgentProfiles(context.Background(), profiles)
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentProfileCheck{
		{Runs: "claude-opus-5"},
		{Problem: "Model claude-haiku-4-5 has no reasoning effort setting on this computer."},
		{Missing: "Codex does not run on this computer."},
	}
	if !reflect.DeepEqual(checks, want) {
		t.Fatalf("checks = %+v, want %+v", checks, want)
	}
	// An effort the model has a setting for but does not offer.
	unoffered, err := app.CheckAgentProfiles(context.Background(), []settings.AgentProfile{
		{Name: "Unoffered", UseFor: "x", Provider: "claude", Model: "claude-opus-5", Effort: "minimal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unoffered) != 1 || !strings.HasPrefix(unoffered[0].Problem, `Model claude-opus-5 does not offer effort "minimal" on this computer. It offers: `) || unoffered[0].Missing != "" {
		t.Fatalf("an effort the model does not offer checks as %+v", unoffered)
	}
	patch := func(list []settings.AgentProfile) map[string]any {
		encoded, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		var raw []any
		if err := json.Unmarshal(encoded, &raw); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"agentProfiles": raw}
	}
	_, err = app.UpdateSettings(context.Background(), patch(profiles))
	if _, message, public := errorsx.PublicDetails(err); !public || message != "Agent profile Effortless: Model claude-haiku-4-5 has no reasoning effort setting on this computer." {
		t.Fatalf("saving a profile its model cannot run = %v", err)
	}
	if _, err := app.UpdateSettings(context.Background(), patch([]settings.AgentProfile{profiles[0], profiles[2]})); err != nil {
		t.Fatalf("saving runnable profiles: %v", err)
	}
}

// instructedForkableThreadOn is forkableThreadOn for a thread a spawn gave
// instructions, which only its creation writes.
func instructedForkableThreadOn(t *testing.T, app *App, sessionID, instructions string) store.Thread {
	t.Helper()
	workspace := t.TempDir()
	writeClaudeProjectSession(t, testProviderHome(t, app), workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"`+sessionID+`","message":{"role":"user","content":"what is the retry policy"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"`+sessionID+`","message":{"role":"assistant","content":[{"type":"text","text":"three attempts"}]}}
`)
	project, err := app.ensureProjectForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: project.ID, ProjectPath: project.Path, WorkspacePath: workspace,
		Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: threadmode.ModeChat,
		SessionRef: sessionID, SpawnInstructions: instructions, Title: "Instructed",
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, thread.ID+":u0", 0, "what is the retry policy", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, app.store, thread.ID, thread.ID+":a0", 0, "three attempts")
	created, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// A spawn's reply records the instructions its thread stores, so the
// spawn's row shows them from its own record: the call's on a fresh
// thread, and a fork's source's before the call's.
func TestASpawnsReplyRecordsTheInstructionsItsThreadRunsWith(t *testing.T) {
	f := newRequestFixture(t)
	installMockClaudeTurns(t, f.app, [][]string{{mockClaudeInitLine}})
	fresh, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "work", Instructions: "Look for bugs."})
	if err != nil || fresh.Instructions != "Look for bugs." {
		t.Fatalf("a fresh spawn's reply = %+v, %v", fresh, err)
	}
	source := instructedForkableThreadOn(t, f.app, "instructed-source", "Review only.")
	fork, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "again", FromThread: source.ID, Instructions: "Be brief."})
	if err != nil || fork.Instructions != "Review only.\n\nBe brief." {
		t.Fatalf("a fork's reply = %+v, %v", fork, err)
	}
	if forked, err := f.app.store.GetThread(fork.ThreadID); err != nil || forked.SpawnInstructions != fork.Instructions {
		t.Fatalf("the fork stores %q, %v; its reply says %q", forked.SpawnInstructions, err, fork.Instructions)
	}
	plain, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "work"})
	if err != nil || plain.Instructions != "" {
		t.Fatalf("a spawn with no instructions replies %+v, %v", plain, err)
	}
}

// The instructions are the request's record from when the spawn named its
// thread: a thread deleted while the spawn waits on it leaves them in the
// reply.
func TestASpawnWhoseThreadIsDeletedDuringItsWaitStillRecordsItsInstructions(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	type result struct {
		ack threadtools.RequestAck
		err error
	}
	done := make(chan result, 1)
	go func() {
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
			Prompt: "work", Instructions: "Look for bugs.", WaitSeconds: 20,
		})
		done <- result{ack, err}
	}()
	var target string
	waitUntilE2E(t, 15*time.Second, "the spawn's thread runs its prompt", func() bool {
		rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
		if err != nil || len(rows) != 1 || rows[0].TargetThreadID == "" {
			return false
		}
		target = rows[0].TargetThreadID
		receipt, found, err := f.app.store.GetThreadRequestReceipt(rows[0].Token)
		return err == nil && found && receipt.State == store.ThreadReceiptRunning && f.app.triage.OpenTurnIndex(target) >= 0
	})
	if _, err := f.app.DeleteThread(target); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if _, err := f.app.store.GetThread(target); err == nil {
		t.Fatal("the spawned thread is still here")
	}
	got := <-done
	if got.err != nil || got.ack.Instructions != "Look for bugs." {
		t.Fatalf("the spawn's reply = %+v, %v; want its thread's instructions", got.ack, got.err)
	}
}

// A paired computer's spawn carries its thread's level: the destination
// stores it with the thread, counts that thread's own workers from it,
// and refuses a spawn whose thread would sit past its own limit. Every
// owned spawn and fork it forwards carries a level, which is what makes
// it owned there (design call 6.3): a spawn arriving without one is a
// plain row. The level counts while the thread is owned: Show in Sidebar
// and a group end it, Remove from Sidebar restores it. A spawn's
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
	if thread.SpawnLevel != 2 || thread.Placement != store.PlacementOwned || thread.SpawnInstructions != "Answer in one line." {
		t.Fatalf("the destination's thread is %s and carries level %d and instructions %q", thread.Placement, thread.SpawnLevel, thread.SpawnInstructions)
	}
	if ack.Instructions != "Answer in one line." {
		t.Fatalf("the spawn's reply records instructions %q", ack.Instructions)
	}
	// A fork there records its source's instructions, which only the
	// destination knows, before the call's.
	instructed := instructedForkableThreadOn(t, pair.dest, "instructed-there", "Review only.")
	forkCtx, cancelFork := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelFork()
	forked, err := pair.adapter().Spawn(forkCtx, pair.callerIdentity(), threadtools.SpawnCall{
		Prompt: "again", FromThread: instructed.ID, FromThreadComputer: pair.computer, ComputerID: pair.computer, Instructions: "Be brief.",
	})
	if err != nil || forked.Instructions != "Review only.\n\nBe brief." {
		t.Fatalf("a fork there replies %+v, %v", forked, err)
	}
	fork, err := pair.dest.store.GetThread(pair.receipt(t, forked.Token).TargetThreadID)
	if err != nil || fork.SpawnLevel != 2 || fork.Placement != store.PlacementOwned {
		t.Fatalf("the fork there is %s at carried level %d, %v; want owned at 2", fork.Placement, fork.SpawnLevel, err)
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

	// The person giving the worker a sidebar row there, or filing it in a
	// group, ends the delegation it came with, and its next session can
	// spawn again; taking it out of the sidebar gives it back.
	requireLevelThere := func(what string, want int) {
		t.Helper()
		if place, err := pair.dest.subThreadPlaceOf(child); err != nil || place.level != want {
			t.Fatalf("the worker %s there: %+v, %v; want level %d", what, place, err, want)
		}
	}
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	requireLevelThere("shown in the sidebar", 0)
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	requireLevelThere("removed from the sidebar", 2)
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

// A paired computer's thread_options answer carries the models it runs but
// does not list, so the caller lists and spawns a profile on a model that
// computer has left off its list, and one inheriting the caller's own model
// left off there too. The list itself stays the computer's shortlist. Not
// parallel: the probe catalog is process-wide.
func TestAProfileOnAModelAPairedComputerRunsButDoesNotListIsOffered(t *testing.T) {
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	// Both CLIs list Opus 4.8 and Sonnet 5; the caller's claude-opus-4-7
	// and claude-opus-4-5 still run on either computer.
	wire := []claude.WireModel{
		{Value: "default", ResolvedModel: "claude-opus-4-8[1m]"},
		{Value: "claude-sonnet-5", ResolvedModel: "claude-sonnet-5"},
		{Value: "opus", ResolvedModel: "claude-opus-4-8"},
	}
	seedClaudeWire(t, pair.dest, wire)
	capture := claudecatalog.ModelCapture{}
	capture.Capture(wire, nil)
	capture.Store(pair.source.claudeProbeModelKey())
	if _, err := pair.source.settings.Update(map[string]any{"agentProfiles": []any{
		map[string]any{"name": "Older Opus", "useFor": "Anything.", "provider": "claude", "model": "claude-opus-4-5"},
		map[string]any{"name": "Like me", "useFor": "Anything."},
	}}); err != nil {
		t.Fatal(err)
	}

	answer := pair.call(t, "thread_options", `{"what":"models"}`)
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "unlisted") {
		t.Errorf("thread_options shows the agent the unlisted models: %s", encoded)
	}
	type computerRow struct {
		ComputerID string                       `json:"computer_id"`
		Providers  []threadtools.ProviderOption `json:"providers"`
		Profiles   []threadtools.ProfileOption  `json:"profiles"`
	}
	var options struct {
		Computers []computerRow `json:"computers"`
	}
	if err := json.Unmarshal(encoded, &options); err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(options.Computers, func(row computerRow) bool { return row.ComputerID == pair.computer })
	if index < 0 {
		t.Fatalf("thread_options has no row for the paired computer: %s", encoded)
	}
	there := options.Computers[index]
	for _, row := range options.Computers {
		for _, option := range row.Providers {
			for _, model := range option.Models {
				if model.Slug == "claude-opus-4-5" || model.Slug == "claude-opus-4-7" {
					t.Errorf("computer %s lists %s, which its CLI does not list", row.ComputerID, model.Slug)
				}
			}
		}
	}
	want := map[string]string{"Older Opus": "claude-opus-4-5", "Like me": "claude-opus-4-7"}
	listed := map[string]string{}
	for _, profile := range there.Profiles {
		listed[profile.Name] = profile.Model
	}
	for name, model := range want {
		if listed[name] != model {
			t.Errorf("the paired computer lists %q on %q, want it on %s; it lists %v", name, listed[name], model, listed)
		}
		spawned := pair.call(t, "thread_spawn", `{"prompt":"work","profile":`+quoteJSON(name)+
			`,"computer_id":`+quoteJSON(pair.computer)+`,"project_id":`+quoteJSON(pair.project)+`}`)
		token, _ := spawned["token"].(string)
		thread, err := pair.dest.store.GetThread(pair.receipt(t, token).TargetThreadID)
		if err != nil {
			t.Fatalf("%s spawned no thread there: %v (%v)", name, err, spawned)
		}
		if thread.Model != model {
			t.Errorf("%s runs %s there, want %s", name, thread.Model, model)
		}
	}
}

// Every seeded profile names a family its provider's grammar reads, so a
// seed follows the newest model of it a computer lists and is never pinned
// to one that goes stale.
func TestTheSeededProfilesNameFamilies(t *testing.T) {
	t.Parallel()
	for _, profile := range settings.DefaultSettings.AgentProfiles {
		if !provider.ParseModelRef(profile.Provider, profile.Model).IsFamily() {
			t.Errorf("seeded profile %s names %s/%s, which is not a family", profile.Name, profile.Provider, profile.Model)
		}
	}
}

// The starting roster a computer answers is its own release's seeds, the
// ones a fresh settings file starts with, and a change to the answer does
// not reach the seeds.
func TestAComputerAnswersItsOwnStartingRoster(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	seeds := app.AgentRosterSeeds()
	if !reflect.DeepEqual(seeds.Profiles, settings.DefaultSettings.AgentProfiles) || !reflect.DeepEqual(seeds.Notes, settings.DefaultSettings.AgentNotes) {
		t.Fatalf("the starting roster is %+v, want the release's seeds", seeds)
	}
	if len(seeds.Profiles) == 0 || len(seeds.Notes) == 0 {
		t.Fatalf("the release seeds no roster: %+v", seeds)
	}
	seeds.Profiles[0].Name, seeds.Notes[0].Text = "Changed", "Changed"
	if again := app.AgentRosterSeeds(); again.Profiles[0].Name == "Changed" || again.Notes[0].Text == "Changed" {
		t.Fatal("a change to one answer reached the seeds")
	}
}

// effortRow is a CLI model row that offers the efforts current models do.
func effortRow(value, resolved string) claude.WireModel {
	return claude.WireModel{Value: value, ResolvedModel: resolved, SupportsEffort: true,
		SupportedEffortLevels: []string{"low", "medium", "high", "xhigh", "max"}}
}

// carriedModels serves the destination of a reach pair through a peer that
// records the model each forwarded thread_spawn carries, and runs changing,
// when set, before the spawn reaches the destination's app.
func carriedModels(t *testing.T, changing func()) (*reachPair, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var carried []string
	record := func(call ThreadPeerCall) {
		var spawn threadtools.SpawnCall
		if err := json.Unmarshal(call.Args, &spawn); err != nil {
			t.Errorf("decode the forwarded spawn: %v", err)
		}
		mu.Lock()
		carried = append(carried, spawn.Model)
		mu.Unlock()
		if changing != nil {
			changing()
		}
	}
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		peer := &ownerOpPeer{App: app}
		peer.spawning.Store(&record)
		return serveAppAs(t, app, peer)
	})
	return pair, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(carried)
	}
}

// A family profile runs the newest model of its family each computer
// lists, so one roster runs Opus 5.5 here and the Opus 4.8 an older CLI
// lists on the paired computer. The spawn there carries the family, which
// the destination resolves against its own list. A family the paired
// computer lists nothing of is left out of its row, and that computer
// refuses its spawn naming the profile. Not parallel: the probe catalog is
// process-wide.
func TestAFamilyProfileRunsTheNewestModelEachComputerLists(t *testing.T) {
	remotetest.Require(t)
	pair, carried := carriedModels(t, nil)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	seedClaudeWire(t, pair.dest, []claude.WireModel{
		effortRow("default", "claude-opus-4-8"),
		effortRow("sonnet", "claude-sonnet-4-6"),
	})
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{
		effortRow("default", "claude-opus-5-5"),
		effortRow("claude-opus-5", "claude-opus-5"),
		effortRow("fable", "claude-fable-5-2"),
	}, nil)
	capture.Store(pair.source.claudeProbeModelKey())
	if pair.source.claudeProbeModelKey() == pair.dest.claudeProbeModelKey() {
		t.Fatal("the two computers share one probe identity, so their lists cannot differ")
	}
	if _, err := pair.source.settings.Update(map[string]any{"agentProfiles": []any{
		map[string]any{"name": "Opus", "useFor": "Anything.", "provider": "claude", "model": "claude-opus", "effort": "high"},
		map[string]any{"name": "Fable", "useFor": "Anything.", "provider": "claude", "model": "claude-fable", "effort": "high"},
	}}); err != nil {
		t.Fatal(err)
	}

	answer := pair.call(t, "thread_options", `{"what":"models"}`)
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var options struct {
		Computers []struct {
			ComputerID string                      `json:"computer_id"`
			Computer   string                      `json:"computer"`
			Local      bool                        `json:"local"`
			Profiles   []threadtools.ProfileOption `json:"profiles"`
		} `json:"computers"`
	}
	if err := json.Unmarshal(encoded, &options); err != nil {
		t.Fatal(err)
	}
	rows := map[bool]map[string]string{}
	there := ""
	for _, row := range options.Computers {
		listed := map[string]string{}
		for _, profile := range row.Profiles {
			listed[profile.Name] = profile.Model
		}
		rows[row.Local] = listed
		if row.ComputerID == pair.computer {
			there = row.Computer
		}
	}
	if want := (map[string]string{"Opus": "claude-opus-5-5", "Fable": "claude-fable-5-2"}); !maps.Equal(rows[true], want) {
		t.Errorf("this computer lists %v, want %v", rows[true], want)
	}
	if want := (map[string]string{"Opus": "claude-opus-4-8"}); !maps.Equal(rows[false], want) {
		t.Errorf("the paired computer lists %v, want %v", rows[false], want)
	}

	spawned := pair.call(t, "thread_spawn", `{"prompt":"work","profile":"Opus","computer_id":`+quoteJSON(pair.computer)+`,"project_id":`+quoteJSON(pair.project)+`}`)
	token, _ := spawned["token"].(string)
	thread, err := pair.dest.store.GetThread(pair.receipt(t, token).TargetThreadID)
	if err != nil {
		t.Fatalf("Opus spawned no thread there: %v (%v)", err, spawned)
	}
	if thread.Model != "claude-opus-4-8" {
		t.Errorf("Opus runs %s there, want claude-opus-4-8", thread.Model)
	}
	if got := carried(); !slices.Equal(got, []string{"claude-opus"}) {
		t.Errorf("the spawn there carried %v, want the family claude-opus", got)
	}

	_, err = pair.callRaw(t, "thread_spawn", `{"prompt":"work","profile":"Fable","computer_id":`+quoteJSON(pair.computer)+`,"project_id":`+quoteJSON(pair.project)+`}`)
	// The computer that would create the thread refuses it, and the caller
	// says which computer answered.
	want := `Agent profile "Fable" cannot run: Claude Code offers no claude-fable model on this computer.`
	if _, message, public := errorsx.PublicDetails(err); !public || !strings.HasPrefix(message, "thread_spawn on "+there) || !strings.Contains(message, want) {
		t.Errorf("Fable's spawn there = %q, want a refusal from %s containing %q", message, there, want)
	}
	if got := carried(); !slices.Equal(got, []string{"claude-opus", "claude-fable"}) {
		t.Errorf("the spawns there carried %v, want both families", got)
	}
}

// A family runs what the computer that creates the thread lists when it
// creates it: a paired computer whose CLI starts listing a newer Opus after
// the caller checked the spawn runs the newer one, for a fresh spawn by
// profile and for a fork. The fork is a Codex caller's of a Claude thread
// there, so its family is read by the source's provider on the computer
// that holds the source. Not parallel: the probe catalog is process-wide.
func TestAFamilySpawnRunsWhatTheCreatingComputerListsAtCreation(t *testing.T) {
	remotetest.Require(t)
	var pair *reachPair
	newer := []claude.WireModel{effortRow("default", "claude-opus-4-9"), effortRow("claude-opus-4-8", "claude-opus-4-8")}
	pair, carried := carriedModels(t, func() {
		capture := claudecatalog.ModelCapture{}
		capture.Capture(newer, nil)
		capture.Store(pair.dest.claudeProbeModelKey())
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	seedClaudeWire(t, pair.dest, []claude.WireModel{effortRow("default", "claude-opus-4-8")})
	if _, err := pair.source.settings.Update(map[string]any{"agentProfiles": []any{
		map[string]any{"name": "Opus", "useFor": "Anything.", "provider": "claude", "model": "claude-opus", "effort": "high"},
	}}); err != nil {
		t.Fatal(err)
	}

	spawned := pair.call(t, "thread_spawn", `{"prompt":"work","profile":"Opus","computer_id":`+quoteJSON(pair.computer)+`,"project_id":`+quoteJSON(pair.project)+`}`)
	token, _ := spawned["token"].(string)
	thread, err := pair.dest.store.GetThread(pair.receipt(t, token).TargetThreadID)
	if err != nil {
		t.Fatalf("Opus spawned no thread there: %v (%v)", err, spawned)
	}
	if thread.Model != "claude-opus-4-9" {
		t.Errorf("Opus runs %s there, want claude-opus-4-9, the newest Opus there when the thread was created", thread.Model)
	}

	caller := pair.caller
	caller.Provider, caller.Model = string(provider.Codex), "gpt-5.5"
	if err := pair.source.store.UpdateThread(caller); err != nil {
		t.Fatal(err)
	}
	seedClaudeWire(t, pair.dest, []claude.WireModel{effortRow("default", "claude-opus-4-8")})
	source := forkableThreadOn(t, pair.dest, "forked-there")
	forked := pair.call(t, "thread_spawn", `{"prompt":"again","from_thread":`+quoteJSON(source.ID)+`,"model":"claude-opus"}`)
	token, _ = forked["token"].(string)
	fork, err := pair.dest.store.GetThread(pair.receipt(t, token).TargetThreadID)
	if err != nil {
		t.Fatalf("the fork made no thread there: %v (%v)", err, forked)
	}
	if fork.Provider != string(provider.Claude) || fork.Model != "claude-opus-4-9" {
		t.Errorf("the fork runs %s/%s there, want claude/claude-opus-4-9", fork.Provider, fork.Model)
	}
	if got := carried(); !slices.Equal(got, []string{"claude-opus", "claude-opus"}) {
		t.Errorf("the spawns there carried %v, want the family both times", got)
	}
}

// The computer that creates the thread is the only one that decides a
// spawn's model: a profile whose family the paired computer lists nothing
// of when the caller looks, and lists by the time it creates the thread,
// runs there. Not parallel: the probe catalog is process-wide.
func TestAProfileThePairedComputerListsOnlyAtCreationRunsThere(t *testing.T) {
	remotetest.Require(t)
	var pair *reachPair
	pair, carried := carriedModels(t, func() {
		capture := claudecatalog.ModelCapture{}
		capture.Capture([]claude.WireModel{effortRow("default", "claude-opus-4-9"), effortRow("sonnet", "claude-sonnet-4-6")}, nil)
		capture.Store(pair.dest.claudeProbeModelKey())
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	seedClaudeWire(t, pair.dest, []claude.WireModel{effortRow("default", "claude-sonnet-4-6")})
	if _, err := pair.source.settings.Update(map[string]any{"agentProfiles": []any{
		map[string]any{"name": "Opus", "useFor": "Anything.", "provider": "claude", "model": "claude-opus", "effort": "high"},
	}}); err != nil {
		t.Fatal(err)
	}

	spawned, err := pair.callRaw(t, "thread_spawn", `{"prompt":"work","profile":"Opus","computer_id":`+quoteJSON(pair.computer)+`,"project_id":`+quoteJSON(pair.project)+`}`)
	if err != nil {
		t.Fatalf("Opus was refused although the paired computer lists an Opus when it creates the thread: %v", err)
	}
	token, _ := spawned["token"].(string)
	thread, err := pair.dest.store.GetThread(pair.receipt(t, token).TargetThreadID)
	if err != nil {
		t.Fatalf("Opus spawned no thread there: %v (%v)", err, spawned)
	}
	if thread.Model != "claude-opus-4-9" {
		t.Errorf("Opus runs %s there, want claude-opus-4-9", thread.Model)
	}
	if got := carried(); !slices.Equal(got, []string{"claude-opus"}) {
		t.Errorf("the spawn there carried %v, want the family claude-opus", got)
	}
}

// The settings check reports the model each profile runs here: the newest
// of a family, the exact model for one that names it, and beside a family
// the listed models its version rule cannot read. Those are logged once
// per change of the catalog, not on each read. It says which models are
// families whether or not their provider runs here, which an editor reads
// for a computer that predates families. Not parallel: the probe catalog
// and the log are process-wide.
func TestTheEditorShowsWhatEachProfileRuns(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": createMockBinary(t, "2.1.284 (Claude Code)"),
		"codexBinaryPath":  filepath.Join(t.TempDir(), "no-codex"),
	}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	seedClaudeWire(t, app, []claude.WireModel{
		effortRow("default", "claude-opus-5-5"),
		effortRow("claude-opus-6-preview", "claude-opus-6-preview"),
	})
	profiles := []settings.AgentProfile{
		{Name: "Opus", UseFor: "x", Provider: "claude", Model: "claude-opus", Effort: "high"},
		{Name: "Pinned", UseFor: "x", Provider: "claude", Model: "claude-opus-5", Effort: "high"},
		{Name: "Sol", UseFor: "x", Provider: "codex", Model: "gpt-sol"},
	}
	want := []AgentProfileCheck{
		{Runs: "claude-opus-5-5", Unreadable: []string{"claude-opus-6-preview"}, Family: true},
		{Runs: "claude-opus-5"},
		{Missing: "Codex does not run on this computer.", Family: true},
	}
	for range 2 {
		checks, err := app.CheckAgentProfiles(context.Background(), profiles)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(checks, want) {
			t.Fatalf("checks = %+v, want %+v", checks, want)
		}
	}
	const line = "model catalog: claude lists claude-opus-6-preview, which its version rule cannot read"
	if got := strings.Count(logged.String(), line); got != 1 {
		t.Fatalf("logged the unreadable model %d times, want once: %q", got, logged.String())
	}
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{effortRow("default", "claude-opus-5-5"), effortRow("claude-opus-7-beta", "claude-opus-7-beta")}, nil)
	capture.Store(app.claudeProbeModelKey())
	if _, err := app.CheckAgentProfiles(context.Background(), profiles); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "model catalog: claude lists claude-opus-7-beta, which its version rule cannot read") {
		t.Fatalf("a changed catalog was not logged: %q", logged.String())
	}
}
