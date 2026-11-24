package app

// runningBackgroundTaskCount counts the background tasks still running on
// one thread.
//
// It reads ListLiveBackgroundTasks, the same composition the tray reads:
// the store query, live Codex subagent launches, and the triage layer's
// in-memory Codex unified-exec tasks, which exist in no table. The tray
// also keeps a completed task on screen for a moment, as a terminal
// sibling row beside its launch; both halves of such a pair are finished
// work and are not counted.
func (a *App) runningBackgroundTaskCount(threadID string) (int, error) {
	items, err := a.ListLiveBackgroundTasks(threadID)
	if err != nil {
		return 0, err
	}
	// A launch row never flips from running; the sibling that settles it
	// names it in CompletionOf. Every sibling in this set is a recent one,
	// since an older one would already have excluded its launch from the
	// query.
	settled := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.CompletionOf != "" {
			settled[item.CompletionOf] = struct{}{}
		}
	}
	// The three sources are disjoint by construction, but a count that
	// double-counted would misreport what a session end stopped, so the
	// union is made explicit rather than assumed.
	counted := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.Status != "running" {
			continue
		}
		if _, done := settled[item.ID]; done {
			continue
		}
		counted[item.ID] = struct{}{}
	}
	return len(counted), nil
}
