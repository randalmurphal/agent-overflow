package main

// GetThreadSlashCommands returns the last-seen Claude slash-command list for
// a thread. Populated from the `slash_commands` field on Claude's system.init
// payload (see internal/provider/claude/protocol.go). Codex has no analogue
// and returns an empty slice.
//
// The binding always returns a non-nil slice so the frontend popover doesn't
// have to null-check; an empty result means "no commands available" and the
// UI renders a quiet empty state.
func (a *App) GetThreadSlashCommands(threadID string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cached, ok := a.threadSlashCommands[threadID]
	if !ok {
		return []string{}, nil
	}
	// Return a defensive copy so the caller can't mutate the cache.
	out := make([]string, len(cached))
	copy(out, cached)
	return out, nil
}
