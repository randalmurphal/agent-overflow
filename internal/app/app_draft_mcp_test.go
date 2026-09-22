package app

import (
	"testing"

	appbrowser "agent-overflow/internal/browser"
)

func TestDraftMovePreservesMCPChoicesAcrossTransitions(t *testing.T) {
	a := draftMoveApp(t)
	a.browser.mcp = appbrowser.NewMCPServer(nil, true)
	t.Cleanup(func() {
		if err := a.browser.mcp.Close(); err != nil {
			t.Error(err)
		}
	})
	from, to := "thr-a", "thr-b"
	for _, enabled := range []bool{false, true, false} {
		preferences := draftMCPPreferences{Threads: enabled, Remote: enabled, Browser: enabled}
		a.applyDraftMCPPreferences(from, preferences)
		snapshot := DraftSnapshot{Content: "move my tools"}
		if err := a.SaveDraft(t.Context(), from, snapshot.Content, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.MoveDraftToThread(t.Context(), from, to, snapshot); err != nil {
			t.Fatal(err)
		}
		if got := a.draftMCPPreferences(to); got != preferences {
			t.Fatalf("destination preferences: %+v, want %+v", got, preferences)
		}
		from, to = to, from
	}
}
