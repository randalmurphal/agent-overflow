package startupprogress

import (
	"encoding/json"
	"net/http"
)

// MigrationsPendingReason is the body's `reason` when a backend refused to
// migrate its database live (docs/specs/app-update.md, the no-live-migration
// rule).
const MigrationsPendingReason = "migrations-pending"

// MigrationsPending is what a backend started to refuse pending migrations
// answers once its store refused: the database's schema version, the
// build's, and how many migrations lie between. The launcher that started it
// migrates the database through a snapshot and a trial, then starts it
// again.
type MigrationsPending struct {
	Database int `json:"database"`
	Build    int `json:"build"`
	Pending  int `json:"pending"`
}

type migrationsPendingBody struct {
	Reason string `json:"reason"`
	MigrationsPending
}

// WriteMigrationsPending answers a bootstrap request with m: 409 Conflict,
// because the backend is reachable and will not start on this database as
// it is.
func WriteMigrationsPending(w http.ResponseWriter, m MigrationsPending) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(migrationsPendingBody{Reason: MigrationsPendingReason, MigrationsPending: m})
}

// ParseMigrationsPending reads a bootstrap answer. It reports true only for
// a 409 whose body is a migrations-pending refusal.
func ParseMigrationsPending(status int, data []byte) (MigrationsPending, bool) {
	if status != http.StatusConflict {
		return MigrationsPending{}, false
	}
	var b migrationsPendingBody
	if err := json.Unmarshal(data, &b); err != nil || b.Reason != MigrationsPendingReason {
		return MigrationsPending{}, false
	}
	return b.MigrationsPending, true
}
