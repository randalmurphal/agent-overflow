package sessionimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
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
		return nil, nil, fmt.Errorf("could not read Antigravity sessions: %w", err)
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

// agyEventsToIR converts replayed ACP ProviderEvents into importir.Events with
// deterministic synthetic source coordinates and a usable row clock. ACP
// `session/load` notifications do not carry per-message timestamps, so each
// event is stamped monotonically from baseTimeMs (the session's reported
// activity time or source file mtime).
func agyEventsToIR(sessionID string, events []provider.ProviderEvent, baseTimeMs int64) []importir.Event {
	if baseTimeMs <= 0 {
		baseTimeMs = time.Now().UnixMilli()
	}
	irEvents := make([]importir.Event, 0, len(events))
	// Chain the SHA-256 digest across all preceding events so even a trailing
	// synthetic turn_complete (with empty role/item/content) carries a unique
	// prefix-dependent cursor fingerprint and refresh detects history divergence.
	chain := sha256.New()
	for i, ev := range events {
		if ev.Timestamp.IsZero() {
			ev.Timestamp = time.UnixMilli(baseTimeMs + int64(i))
		}
		if ev.Kind == provider.EventTurnComplete && ev.TurnComplete == nil {
			ev.TurnComplete = &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
		}
		_, _ = fmt.Fprintf(chain, "%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
			i, ev.Kind, ev.Role, ev.ItemID, ev.ItemType, ev.Content, string(ev.Meta))
		fp := hex.EncodeToString(chain.Sum(nil)[:8])
		irEvents = append(irEvents, importir.Event{
			ProviderEvent: ev,
			SourceUUID:    fmt.Sprintf("acp:%s:%d:%s", sessionID, i, fp),
		})
	}
	return irEvents
}

func importAgy(ctx context.Context, d Deps, row Row, proj store.Project) (ImportOutcome, error) {
	if d.AgyReplaySession == nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: Antigravity replay unavailable")
	}
	events, err := d.AgyReplaySession(ctx, row.SessionID, row.ProjectPath)
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: replay agy session %s: %w", row.SessionID, err)
	}
	baseTimeMs := row.LastActivityAt
	if baseTimeMs <= 0 {
		baseTimeMs = row.CreatedAt
	}
	irEvents := agyEventsToIR(row.SessionID, events, baseTimeMs)
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
