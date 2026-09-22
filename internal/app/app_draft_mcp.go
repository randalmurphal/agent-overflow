package app

// These switches belong to the conversation. Provider MCP configuration belongs
// to the destination workspace and is resolved there when the provider starts.
type draftMCPPreferences struct {
	Threads bool `json:"threads"`
	Remote  bool `json:"remote"`
	Browser bool `json:"browser"`
}

func (a *App) draftMCPPreferences(threadID string) draftMCPPreferences {
	return draftMCPPreferences{
		Threads: a.threadMCPServer().ThreadEnabled(threadID),
		Remote:  a.remoteMCPServer().ThreadEnabled(threadID),
		Browser: a.browser.mcp == nil || a.browser.mcp.ThreadEnabled(threadID),
	}
}

func (a *App) applyDraftMCPPreferences(threadID string, preferences draftMCPPreferences) {
	a.threadMCPServer().SetThreadEnabled(threadID, preferences.Threads)
	a.remoteMCPServer().SetThreadEnabled(threadID, preferences.Remote)
	if a.browser.mcp != nil {
		a.browser.mcp.SetThreadEnabled(threadID, preferences.Browser)
	}
}
