package app

import (
	"strings"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// Every server the app registers on a session itself must have a line in
// the guide, or a new server ships with tools the model is never told about.
func TestAgentGuideNamesEveryAppManagedServer(t *testing.T) {
	t.Parallel()
	named := make(map[string]bool, len(agentGuideServers))
	for _, server := range agentGuideServers {
		if !strings.Contains(server.sentence, "`"+server.name+"`") {
			t.Errorf("guide sentence for %s does not name the server: %q", server.name, server.sentence)
		}
		named[server.name] = true
	}
	for _, name := range appManagedMCPServers {
		if !named[name] {
			t.Errorf("app-managed server %s has no sentence in the agent guide", name)
		}
	}
	if len(agentGuideServers) != len(appManagedMCPServers) {
		t.Errorf("guide lists %d servers, app manages %d", len(agentGuideServers), len(appManagedMCPServers))
	}
}

// The tools paragraph names exactly the servers that are on, in the guide's
// order, and disappears entirely when none is.
func TestAgentGuideTextListsOnlyTheServersThatAreOn(t *testing.T) {
	t.Parallel()
	none := agentGuideText(nil)
	if none != agentGuideRendering {
		t.Fatalf("guide with no servers = %q, want the rendering paragraph alone", none)
	}
	for _, name := range appManagedMCPServers {
		if strings.Contains(none, name) {
			t.Errorf("guide with no servers still names %s", name)
		}
	}

	browserOnly := agentGuideText(map[string]bool{appbrowser.ServerName: true})
	if !strings.HasPrefix(browserOnly, agentGuideRendering+"\n\n"+agentGuideToolsOpening) {
		t.Fatalf("guide does not open its tools paragraph after the rendering one:\n%s", browserOnly)
	}
	if !strings.HasSuffix(browserOnly, agentGuideToolsClosing) {
		t.Fatalf("guide does not close its tools paragraph:\n%s", browserOnly)
	}
	if !strings.Contains(browserOnly, "`"+appbrowser.ServerName+"`") {
		t.Errorf("browser-only guide does not name the browser server:\n%s", browserOnly)
	}
	for _, name := range []string{threadMCPName, remoteMCPName} {
		if strings.Contains(browserOnly, name) {
			t.Errorf("browser-only guide names %s:\n%s", name, browserOnly)
		}
	}

	all := agentGuideText(map[string]bool{appbrowser.ServerName: true, threadMCPName: true, remoteMCPName: true})
	last := -1
	for _, server := range agentGuideServers {
		at := strings.Index(all, server.sentence)
		if at < 0 {
			t.Fatalf("full guide lacks the %s sentence:\n%s", server.name, all)
		}
		if at < last {
			t.Errorf("full guide lists %s out of order", server.name)
		}
		last = at
	}
	if strings.Count(all, agentGuideToolsOpening) != 1 {
		t.Errorf("full guide opens its tools paragraph %d times", strings.Count(all, agentGuideToolsOpening))
	}
}

// The on set is the effective one, not the registered one: the browser
// entry is registered while its switch is off so the list can be turned on
// live, and the thread-tools entry stays through its switch too.
func TestAgentGuideServersOnFollowTheEffectiveSwitches(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	registered := map[string]any{
		appbrowser.ServerName: map[string]any{"url": "http://127.0.0.1/browser/" + token},
		threadMCPName:         map[string]any{"url": "http://127.0.0.1/threads/" + token},
		remoteMCPName:         map[string]any{"url": "http://127.0.0.1/remote/" + token},
	}

	on := app.agentGuideServersOn(thread.ID, registered)
	for _, name := range appManagedMCPServers {
		if !on[name] {
			t.Errorf("%s registered and switched on, but not in the guide", name)
		}
	}

	if _, err := app.settings.Update(map[string]any{"browserEnabled": false}); err != nil {
		t.Fatalf("disable browser: %v", err)
	}
	if app.agentGuideServersOn(thread.ID, registered)[appbrowser.ServerName] {
		t.Error("browser named in the guide while the global switch is off")
	}
	if _, err := app.settings.Update(map[string]any{"browserEnabled": true}); err != nil {
		t.Fatalf("enable browser: %v", err)
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, false)
	if app.agentGuideServersOn(thread.ID, registered)[appbrowser.ServerName] {
		t.Error("browser named in the guide while the thread switch is off")
	}

	if _, err := app.settings.Update(map[string]any{"threadToolsEnabled": false}); err != nil {
		t.Fatalf("disable thread tools: %v", err)
	}
	if app.agentGuideServersOn(thread.ID, registered)[threadMCPName] {
		t.Error("thread tools named in the guide while the switch is off")
	}

	if on := app.agentGuideServersOn(thread.ID, nil); len(on) != 0 {
		t.Errorf("nothing registered, yet the guide names %v", on)
	}
}

// The setting is the master switch, and a workflow session never carries
// the guide: it runs under its workflow's own prompt and structured output,
// and the exclusion is by mode so a thread whose attempt row is not attached
// yet is excluded too.
func TestAgentGuideForHonorsTheSettingAndSkipsWorkflowThreads(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, _ := remoteMCPThread(t, app, string(provider.Claude))

	guide, err := app.agentGuideFor(thread, nil)
	if err != nil {
		t.Fatalf("agentGuideFor: %v", err)
	}
	if guide != agentGuideRendering {
		t.Fatalf("default guide = %q, want the rendering paragraph", guide)
	}

	tui := thread
	tui.Provider = string(provider.ClaudeTUI)
	if guide, err := app.agentGuideFor(tui, nil); err != nil || guide != agentGuideRendering {
		t.Fatalf("claude-tui guide = %q, %v; want the rendering paragraph", guide, err)
	}

	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": false}); err != nil {
		t.Fatalf("disable guide: %v", err)
	}
	if guide, err := app.agentGuideFor(thread, nil); err != nil || guide != "" {
		t.Fatalf("guide with the setting off = %q, %v; want none", guide, err)
	}
	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": true}); err != nil {
		t.Fatalf("enable guide: %v", err)
	}

	phase := store.Thread{ID: "phase-thread", ProjectID: thread.ProjectID, Provider: string(provider.Claude), Mode: threadmode.ModeWorkflow}
	if _, ok, err := app.deriveCallerScope(phase); err != nil || ok {
		t.Fatalf("unattached workflow thread scope: ok=%t err=%v; the fixture must not resolve a phase", ok, err)
	}
	if guide, err := app.agentGuideFor(phase, nil); err != nil || guide != "" {
		t.Fatalf("workflow thread guide = %q, %v; want none", guide, err)
	}
}

// Codex reads no server instructions from the handshake, so its developer
// instructions carry the app guide and then each on server's own guide.
func TestCodexDeveloperInstructionsComposeTheGuides(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	thread, token := remoteMCPThread(t, app, string(provider.Codex))
	registered := map[string]any{
		appbrowser.ServerName: map[string]any{"url": "http://127.0.0.1/browser/" + token},
		threadMCPName:         map[string]any{"url": "http://127.0.0.1/threads/" + token},
	}

	got := app.codexDeveloperInstructions(thread.ID, "APP GUIDE", registered)
	want := strings.Join([]string{
		"APP GUIDE",
		appbrowser.Instructions(),
		app.threadToolsServer().Instructions(app.threadToolsShape(thread.ID)),
	}, "\n\n")
	if got != want {
		t.Fatalf("composed developer instructions:\n%s\nwant:\n%s", got, want)
	}

	app.browser.mcp.SetThreadEnabled(thread.ID, false)
	if got := app.codexDeveloperInstructions(thread.ID, "APP GUIDE", registered); strings.Contains(got, appbrowser.Instructions()) {
		t.Error("browser guide composed while the thread's browser switch is off")
	}

	if got := app.codexDeveloperInstructions(thread.ID, "", nil); got != "" {
		t.Fatalf("nothing on and no app guide, yet developer instructions = %q", got)
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, true)
	if got := app.codexDeveloperInstructions(thread.ID, "", registered); !strings.HasPrefix(got, appbrowser.Instructions()) {
		t.Fatalf("with the app guide off the server guides must still ride alone; got %q", got)
	}
}
