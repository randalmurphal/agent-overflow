package main

import "agent-overflow/internal/uitrace"

// uiTrace returns the lazy-initialized Tracer keyed on a.configDir.
// Construction is one-shot so subsequent calls reuse the same Tracer
// (and its appendMutex). Returning the same error on every retry — see
// uitrace.New — means a misconfigured App (empty configDir) fails loudly
// on every binding call instead of silently no-op'ing after the first.
func (a *App) uiTrace() (*uitrace.Tracer, error) {
	a.uiTraceOnce.Do(func() {
		a.uiTracer, a.uiTraceErr = uitrace.New(a.configDir)
	})
	return a.uiTracer, a.uiTraceErr
}

// GetUIRenderTracePath returns the dev trace file path used by
// AppendUIRenderTraceBatch. The frontend exposes it through the console trace
// API so a debug run can be inspected after a visual glitch.
func (a *App) GetUIRenderTracePath() (string, error) {
	t, err := a.uiTrace()
	if err != nil {
		return "", err
	}
	return t.Path(), nil
}

// AppendUIRenderTraceBatch appends compact dev-only UI render trace records.
// The frontend batches calls so rendering never waits on disk. The binding
// validates each line because it writes directly into the user's config
// directory.
func (a *App) AppendUIRenderTraceBatch(lines []string) (string, error) {
	t, err := a.uiTrace()
	if err != nil {
		return "", err
	}
	return t.Append(lines)
}
