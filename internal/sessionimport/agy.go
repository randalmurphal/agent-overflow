package sessionimport

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/store"
)

// AgySessionSummary is one Antigravity session reported by `session/list`
// over ACP, passed through Deps.AgyListSessions so sessionimport stays
// decoupled from process spawning in tests.
type AgySessionSummary struct {
	SessionID      string
	Title          string
	WorkDir        string
	UpdatedAtMs    int64
	SourcePath     string
	IndexedProfile importir.ModelProfile
}

func scanAgy(ctx context.Context, d Deps, known map[store.ProviderSessionRef]string) ([]Row, []importir.Warning, error) {
	if d.AgyListSessions == nil {
		return nil, nil, fmt.Errorf("no Antigravity home was found on this machine")
	}
	sessions, err := d.AgyListSessions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("Antigravity sessions could not be read: %w", err)
	}
	rows := make([]Row, 0, len(sessions))
	for _, s := range sessions {
		sessionID := strings.TrimSpace(s.SessionID)
		if sessionID == "" {
			continue
		}
		key := store.ProviderSessionRef{Provider: ProviderAgy, SessionID: sessionID}
		if _, taken := known[key]; taken {
			continue
		}
		sourcePath := strings.TrimSpace(s.SourcePath)
		if sourcePath == "" {
			sourcePath = "acp:" + sessionID
		}
		rows = append(rows, Row{
			ID:             RowKey(ProviderAgy, sessionID),
			Provider:       ProviderAgy,
			SessionID:      sessionID,
			Title:          strings.TrimSpace(s.Title),
			ProjectPath:    strings.TrimSpace(s.WorkDir),
			CreatedAt:      s.UpdatedAtMs,
			LastActivityAt: s.UpdatedAtMs,
			SourcePath:     sourcePath,
			IndexedProfile: s.IndexedProfile,
		})
	}
	return rows, nil, nil
}

func importAgy(ctx context.Context, d Deps, row Row, proj store.Project) (ImportOutcome, error) {
	if d.AgyReplaySession == nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: Antigravity replay unavailable")
	}
	events, err := d.AgyReplaySession(ctx, row.SessionID, row.ProjectPath)
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: replay agy session %s: %w", row.SessionID, err)
	}
	irEvents := make([]importir.Event, 0, len(events))
	for i, ev := range events {
		irEvents = append(irEvents, importir.Event{
			ProviderEvent: ev,
			SourceUUID:    fmt.Sprintf("acp:%s:%d", row.SessionID, i),
		})
	}
	im := newSessionImporter(d.Store, row, proj)
	if len(irEvents) == 0 {
		return im.outcome(), nil
	}
	if err := im.add(branchPlan{
		title:          importedTitle(row.Title),
		sessionRef:     row.SessionID,
		events:         irEvents,
		profile:        row.IndexedProfile,
		lastActivityAt: row.LastActivityAt,
	}); err != nil {
		return ImportOutcome{}, err
	}
	return im.outcome(), nil
}
