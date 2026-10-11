package app

import (
	"strings"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// The app guide is the short text Agent Overflow appends to an interactive
// session's system prompt (settings.AgentGuideEnabled): what the chat pane
// renders, and which of the app's own tool servers the session has on. It
// rides `--append-system-prompt-file` on both Claude providers and the
// composed `developerInstructions` on Codex (docs/specs/prompt-tool-overrides.md).
//
// Workflow sessions (phases and units) never carry it: they run under the
// workflow's own prompt and structured output, and their tool servers are
// granted per phase rather than per conversation.

// agentGuideRendering is the first paragraph, stated against the renderer
// in frontend/src/lib/components/chat (GFM extensions, path links, local
// images, .html links through internal/filepreview).
const agentGuideRendering = "You are running inside Agent Overflow, a desktop app that shows this conversation in a chat pane. Replies render as GitHub-flavored Markdown: tables, task lists, `> [!NOTE]` alerts, footnotes, highlighted code fences, Mermaid in ```mermaid fences (openable full-screen), and LaTeX between `$` or `$$`. Raw HTML shows as text. File paths you write become links that open the user's editor (`path:line:col` works); a link to an `.html` file opens it as a page in their browser. `![alt](path.png)` shows a local image inline, SVG included."

// agentGuideServer is one app-managed MCP server's sentence in the guide's
// tools paragraph, listed in this order. Every name in appManagedMCPServers
// has an entry here (TestAgentGuideNamesEveryAppManagedServer).
type agentGuideServer struct {
	name     string
	sentence string
}

var agentGuideServers = []agentGuideServer{
	{appbrowser.ServerName, "`" + appbrowser.ServerName + "` drives a browser in a pane beside this chat; use it to show the user a page or an HTML file you made (make the page visible; screenshots reach only you)."},
	{threadMCPName, "`" + threadMCPName + "` reads, starts and messages the user's other conversations."},
	{remoteMCPName, "`" + remoteMCPName + "` runs commands on the user's other computers."},
}

const (
	agentGuideToolsOpening = "App tools: "
	agentGuideToolsClosing = " Load a server's tools when the task calls for them."
)

// agentGuideText renders the guide for a session whose effective tool
// servers are the names in `on`. With none on, the tools paragraph is
// omitted rather than left as an empty heading.
func agentGuideText(on map[string]bool) string {
	sentences := make([]string, 0, len(agentGuideServers))
	for _, server := range agentGuideServers {
		if on[server.name] {
			sentences = append(sentences, server.sentence)
		}
	}
	if len(sentences) == 0 {
		return agentGuideRendering
	}
	return agentGuideRendering + "\n\n" + agentGuideToolsOpening + strings.Join(sentences, " ") + agentGuideToolsClosing
}

// agentGuideServersOn names the app-managed servers a session will actually
// have tools from. `registered` is the server map the session is spawned
// with; registration alone is not enough for the browser server, which is
// registered whenever the engine exists so the tool list can be switched on
// live, and for the thread tools, whose entry stays while the switch is off.
func (a *App) agentGuideServersOn(threadID string, registered map[string]any) map[string]bool {
	on := make(map[string]bool, len(agentGuideServers))
	if registered[appbrowser.ServerName] != nil && a.currentSettings().BrowserEnabled &&
		a.browser.mcp != nil && a.browser.mcp.ThreadEnabled(threadID) {
		on[appbrowser.ServerName] = true
	}
	if registered[threadMCPName] != nil && a.threadToolsEnabledFor(threadID) {
		on[threadMCPName] = true
	}
	if registered[remoteMCPName] != nil {
		on[remoteMCPName] = true
	}
	return on
}

// agentGuideFor is the guide one session carries, or "" when the setting
// is off, the thread is a workflow session, or the provider is not one the
// guide reaches.
func (a *App) agentGuideFor(thread store.Thread, registered map[string]any) (string, error) {
	switch thread.Provider {
	case string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex):
	default:
		return "", nil
	}
	if !a.currentSettings().AgentGuideEnabled {
		return "", nil
	}
	// Every workflow-mode thread (a phase or a unit), whether or not its
	// attempt row has been attached yet: a workflow session runs under its
	// workflow's prompt and structured output, with tool servers granted
	// per phase, so the chat-pane guide is not its contract.
	if thread.Mode == threadmode.ModeWorkflow {
		return "", nil
	}
	return agentGuideText(a.agentGuideServersOn(thread.ID, registered)), nil
}

// codexDeveloperInstructions composes everything Codex reads as developer
// instructions: the app guide, then the guide of each on server that
// Claude would read from the MCP handshake instead. Codex never shows a
// server's `instructions` to the model, so this is the only way those
// guides reach it. Empty when nothing applies, and the caller then omits
// the field so the cwd's configured instructions stand unchanged.
func (a *App) codexDeveloperInstructions(threadID, appGuide string, registered map[string]any) string {
	on := a.agentGuideServersOn(threadID, registered)
	parts := make([]string, 0, 3)
	if appGuide != "" {
		parts = append(parts, appGuide)
	}
	if on[appbrowser.ServerName] {
		parts = append(parts, appbrowser.Instructions())
	}
	if on[threadMCPName] {
		parts = append(parts, a.threadToolsServer().Instructions(a.threadToolsShape(threadID)))
	}
	return strings.Join(parts, "\n\n")
}
