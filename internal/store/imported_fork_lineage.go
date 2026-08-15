package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const (
	ImportedForkLineageAmbiguous = "imported-fork-parent-ambiguous"
	ImportedForkLineageSelf      = "imported-fork-parent-self"
	ImportedForkLineageCycle     = "imported-fork-lineage-cycle"
)

// ImportedForkLineageWarning describes provider ancestry that cannot safely
// become an AO thread link. Reconciliation leaves the conversation intact and
// omits only the suspect link; callers surface the warning with the import.
type ImportedForkLineageWarning struct {
	ThreadID string
	Code     string
	Message  string
}

type importedForkSource struct {
	threadID        string
	provider        string
	sessionID       string
	parentSessionID string
}

// ReconcileImportedForkLineage maps provider fork ids to AO thread ids after
// provider/sessionID has just been imported or otherwise became available.
//
// Parent and child sessions may be imported in either order. The durable
// source_parent_session_id keeps the unresolved edge when only the child is
// present; every later import reruns this method, so the edge appears as soon
// as its parent does. The update is global and transactional, which also makes
// a multi-generation Import All independent of worker scheduling.
//
// Only imported threads carrying explicit provider ancestry are updated.
// AO-native fork links remain owned by the native fork path. Ambiguous,
// self-referential, and cyclic provider metadata is never guessed at: history
// remains available, the unsafe imported links are cleared, and a warning is
// returned.
func (s *Store) ReconcileImportedForkLineage(
	providerName, sessionID string,
) ([]ImportedForkLineageWarning, error) {
	if providerName != "claude" && providerName != "codex" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidImportProvider, providerName)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("store: reconcile imported fork lineage: source session id is required")
	}

	// Almost every provider session is a root. Avoid turning Import All into
	// an O(sessions²) global lineage pass: only a session that IS a fork or
	// one with already-imported children can change an edge. Both predicates
	// use v63's narrow provider/session indexes.
	var relevant int
	if err := s.db.QueryRow(
		`SELECT EXISTS (
		     SELECT 1 FROM thread_import_state
		      WHERE provider = ? AND source_session_id = ?
		        AND source_parent_session_id <> ''
		     UNION ALL
		     SELECT 1 FROM thread_import_state
		      WHERE provider = ? AND source_parent_session_id = ?
		 )`,
		providerName, sessionID, providerName, sessionID,
	).Scan(&relevant); err != nil {
		return nil, fmt.Errorf("store: inspect imported fork lineage relevance: %w", err)
	}
	if relevant == 0 {
		return nil, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin imported fork lineage reconciliation: %w", err)
	}
	defer tx.Rollback()

	sources, err := loadImportedForkSources(tx)
	if err != nil {
		return nil, err
	}
	claims, err := loadProviderSessionClaims(tx)
	if err != nil {
		return nil, err
	}
	graph, err := loadForkGraph(tx)
	if err != nil {
		return nil, err
	}

	desired := make(map[string]string, len(sources))
	sourcesByThread := make(map[string]importedForkSource, len(sources))
	warnings := make([]ImportedForkLineageWarning, 0)
	for _, source := range sources {
		sourcesByThread[source.threadID] = source
		key := ProviderSessionRef{Provider: source.provider, SessionID: source.parentSessionID}
		parents := sortedClaimants(claims[key])
		switch len(parents) {
		case 0:
			// The parent has not been imported yet. Clearing a stale resolved
			// link is important when a parent was deleted; the durable source id
			// lets a later re-import restore it.
			desired[source.threadID] = ""
		case 1:
			if parents[0] == source.threadID {
				desired[source.threadID] = ""
				warnings = append(warnings, ImportedForkLineageWarning{
					ThreadID: source.threadID,
					Code:     ImportedForkLineageSelf,
					Message: fmt.Sprintf(
						"Provider session %s:%s claims itself as its fork parent; its history was imported without a parent link.",
						source.provider, source.sessionID),
				})
				continue
			}
			desired[source.threadID] = parents[0]
		default:
			desired[source.threadID] = ""
			warnings = append(warnings, ImportedForkLineageWarning{
				ThreadID: source.threadID,
				Code:     ImportedForkLineageAmbiguous,
				Message: fmt.Sprintf(
					"Provider session %s:%s has fork parent %s claimed by multiple threads; its history was imported without guessing a parent link.",
					source.provider, source.sessionID, source.parentSessionID),
			})
		}
	}

	// Reconciliation owns these imported edges, including clearing an edge
	// whose parent disappeared or became ambiguous.
	for child, parent := range desired {
		if parent == "" {
			delete(graph, child)
		} else {
			graph[child] = parent
		}
	}
	cycleNodes := forkCycleNodes(graph)
	for child, parent := range desired {
		if parent == "" || !cycleNodes[child] {
			continue
		}
		desired[child] = ""
		source := sourcesByThread[child]
		warnings = append(warnings, ImportedForkLineageWarning{
			ThreadID: child,
			Code:     ImportedForkLineageCycle,
			Message: fmt.Sprintf(
				"Provider session %s:%s has fork metadata that would create a thread-lineage cycle; its history was imported without the cyclic parent link.",
				source.provider, source.sessionID),
		})
	}

	children := make([]string, 0, len(desired))
	for child := range desired {
		children = append(children, child)
	}
	sort.Strings(children)
	for _, child := range children {
		var parent any
		if desired[child] != "" {
			parent = desired[child]
		}
		if _, err := tx.Exec(
			`UPDATE threads SET forked_from_thread_id = ? WHERE id = ?`, parent, child,
		); err != nil {
			return nil, fmt.Errorf("store: reconcile imported fork parent for thread %s: %w", child, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit imported fork lineage reconciliation: %w", err)
	}
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].ThreadID != warnings[j].ThreadID {
			return warnings[i].ThreadID < warnings[j].ThreadID
		}
		return warnings[i].Code < warnings[j].Code
	})
	return warnings, nil
}

func loadImportedForkSources(tx *sql.Tx) ([]importedForkSource, error) {
	rows, err := tx.Query(
		`SELECT thread_id, provider, source_session_id, source_parent_session_id
		   FROM thread_import_state
		  WHERE source_parent_session_id <> ''
		  ORDER BY thread_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list imported fork sources: %w", err)
	}
	defer rows.Close()

	var sources []importedForkSource
	for rows.Next() {
		var source importedForkSource
		if err := rows.Scan(
			&source.threadID, &source.provider, &source.sessionID, &source.parentSessionID,
		); err != nil {
			return nil, fmt.Errorf("store: scan imported fork source: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: imported fork source rows: %w", err)
	}
	return sources, nil
}

func loadProviderSessionClaims(tx *sql.Tx) (map[ProviderSessionRef]map[string]struct{}, error) {
	rows, err := tx.Query(
		`SELECT provider, session_id, thread_id FROM (
		     SELECT provider, source_session_id AS session_id, thread_id
		       FROM thread_import_state
		      WHERE source_session_id <> ''
		     UNION ALL
		     SELECT CASE provider WHEN 'claude-tui' THEN 'claude' ELSE provider END,
		            session_ref, id
		       FROM threads
		      WHERE COALESCE(session_ref, '') <> ''
		 )`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list provider session claims for fork lineage: %w", err)
	}
	defer rows.Close()

	claims := make(map[ProviderSessionRef]map[string]struct{})
	for rows.Next() {
		var providerName, sessionID, threadID string
		if err := rows.Scan(&providerName, &sessionID, &threadID); err != nil {
			return nil, fmt.Errorf("store: scan provider session claim for fork lineage: %w", err)
		}
		key := ProviderSessionRef{Provider: providerName, SessionID: sessionID}
		if claims[key] == nil {
			claims[key] = make(map[string]struct{})
		}
		claims[key][threadID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: provider session claim rows for fork lineage: %w", err)
	}
	return claims, nil
}

func loadForkGraph(tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.Query(
		`SELECT id, forked_from_thread_id
		   FROM threads
		  WHERE COALESCE(forked_from_thread_id, '') <> ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread fork graph: %w", err)
	}
	defer rows.Close()

	graph := make(map[string]string)
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("store: scan thread fork graph: %w", err)
		}
		graph[child] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: thread fork graph rows: %w", err)
	}
	return graph, nil
}

func sortedClaimants(claims map[string]struct{}) []string {
	result := make([]string, 0, len(claims))
	for threadID := range claims {
		result = append(result, threadID)
	}
	sort.Strings(result)
	return result
}

func forkCycleNodes(graph map[string]string) map[string]bool {
	done := make(map[string]bool, len(graph))
	cycles := make(map[string]bool)
	starts := make([]string, 0, len(graph))
	for child := range graph {
		starts = append(starts, child)
	}
	sort.Strings(starts)

	for _, start := range starts {
		if done[start] {
			continue
		}
		path := make([]string, 0)
		position := make(map[string]int)
		for node := start; node != "" && !done[node]; node = graph[node] {
			if at, found := position[node]; found {
				for _, cycleNode := range path[at:] {
					cycles[cycleNode] = true
				}
				break
			}
			position[node] = len(path)
			path = append(path, node)
		}
		for _, node := range path {
			done[node] = true
		}
	}
	return cycles
}
