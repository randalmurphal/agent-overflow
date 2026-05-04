package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// triageTestProjectID is the stable project id every triage test uses
// when inserting test threads. Wire it up via ensureTriageProject so
// the FK on threads.project_id resolves.
const triageTestProjectID = "triage-test-project"

// ensureTriageProject seeds the default project row used by the triage
// package's tests. Idempotent: no-op if already present.
func ensureTriageProject(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.GetProject(triageTestProjectID); err == nil {
		return
	}
	now := time.Now().UnixMilli()
	if err := st.CreateProject(store.Project{
		ID:        triageTestProjectID,
		Path:      "/tmp/triage",
		Name:      "Triage Tests",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed triage test project: %v", err)
	}
}

func normalTurnCompleteMeta() *provider.WireTurnCompleteMeta {
	return &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
}
