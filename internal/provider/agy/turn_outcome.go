package agy

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

// cortexStepTypeErrorMessage is agy's CORTEX_STEP_TYPE_ERROR_MESSAGE enum value
// stored in conversations/<sessionId>.db `steps.step_type`.
const (
	cortexStepTypePlannerResponse = 15
	cortexStepTypeErrorMessage    = 17
	cortexStepTypeCheckpoint      = 23
	maxErrorPayloadScanBytes      = 64 * 1024
)

var (
	reQuotaExhausted     = regexp.MustCompile(`(?i)QUOTA_EXHAUSTED|quota\s+exhausted`)
	reResourceExhausted  = regexp.MustCompile(`(?i)RESOURCE_EXHAUSTED|\b429\b|rate\s*limit`)
	reInvalidThinkingSig = regexp.MustCompile(`(?i)INVALID_THINKING_SIGNATURE|invalid\s+thinking\s+signature`)
	reUpstreamServerErr  = regexp.MustCompile(`(?i)\b50[0234]\b|INTERNAL|UNAVAILABLE|DEADLINE_EXCEEDED`)
	reMidTurnNotFound    = regexp.MustCompile(`(?i)\b404\b|NOT_FOUND`)
	reHumanReadableASCII = regexp.MustCompile(`[ -~]{6,}`)
)

func validateConversationSessionID(sessionID string) error {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" || trimmed != sessionID ||
		trimmed == "." || trimmed == ".." ||
		strings.ContainsAny(trimmed, `/\`) ||
		strings.Contains(trimmed, "..") {
		return fmt.Errorf("invalid conversation session id %q", sessionID)
	}
	return nil
}

func conversationDBPath(appDataDir, sessionID string) (string, error) {
	if err := validateConversationSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(appDataDir, "conversations", sessionID+".db"), nil
}

var sqliteDSNEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// sqliteFileDSN builds a cross-platform SQLite file URI DSN that preserves
// Windows drive letters (e.g. file:C:/Users/...) while escaping URI delimiters.
func sqliteFileDSN(path, query string) string {
	slashed := filepath.ToSlash(path)
	return "file:" + sqliteDSNEscaper.Replace(slashed) + "?" + query
}

func openConversationDBReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", sqliteFileDSN(path, "mode=ro&_pragma=busy_timeout(100)"))
}

// readMaxStepIndex returns the highest `idx` in `steps` before a prompt starts.
// When the conversation DB does not exist yet (brand-new session before Turn 1),
// it returns (-1, true) so Turn 1 step_type=17 errors at idx >= 0 are still caught.
func readMaxStepIndex(appDataDir, sessionID string) (int, bool) {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return 0, false
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return -1, true
		}
		return 0, false
	}
	defer func() { _ = db.Close() }()

	var maxIdx sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
		return 0, false
	}
	if !maxIdx.Valid {
		return -1, true
	}
	return int(maxIdx.Int64), true
}

// readMaxCheckpointIndex returns the highest `idx` of a compaction checkpoint
// (step_type = 23) in conversations/<sessionId>.db, or -1 if none exists yet.
func readMaxCheckpointIndex(appDataDir, sessionID string) int {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return -1
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return -1
	}
	defer func() { _ = db.Close() }()

	var maxIdx sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps WHERE step_type = ?`, cortexStepTypeCheckpoint).Scan(&maxIdx); err != nil || !maxIdx.Valid {
		return -1
	}
	return int(maxIdx.Int64)
}

// detectNewCheckpoint checks whether agy wrote a compaction CHECKPOINT step
// (step_type = 23) with idx > afterIdx.
func detectNewCheckpoint(appDataDir, sessionID string, afterIdx int) (int, *acp.CompactionEvent) {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return afterIdx, nil
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return afterIdx, nil
	}
	defer func() { _ = db.Close() }()

	var idx sql.NullInt64
	if err := db.QueryRow(
		`SELECT MAX(idx) FROM steps WHERE step_type = ? AND idx > ?`,
		cortexStepTypeCheckpoint, afterIdx,
	).Scan(&idx); err != nil || !idx.Valid {
		return afterIdx, nil
	}
	newIdx := int(idx.Int64)
	return newIdx, &acp.CompactionEvent{
		ItemID: fmt.Sprintf("agy-compact-%d", newIdx),
	}
}

// inspectTurnErrorStep checks whether agy wrote a CORTEX_STEP_TYPE_ERROR_MESSAGE
// (step_type = 17) row with idx > sinceIdx during the prompt that just ended.
func inspectTurnErrorStep(appDataDir, sessionID string, sinceIdx int) *acp.PromptOutcomeOverride {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return nil
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(
		`SELECT idx, step_type, COALESCE(error_details, X''), COALESCE(step_payload, X'') FROM steps WHERE idx > ? AND step_type = ? ORDER BY idx DESC LIMIT 1`,
		sinceIdx, cortexStepTypeErrorMessage,
	)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return nil
	}
	var idx, stepType int
	var errDetails, stepPayload []byte
	if err := rows.Scan(&idx, &stepType, &errDetails, &stepPayload); err != nil {
		return nil
	}
	// If agy retried and succeeded with a subsequent planner response after
	// this error step, do not mark the whole turn failed.
	var laterPlanner sql.NullInt64
	if err := db.QueryRow(
		`SELECT MAX(idx) FROM steps WHERE idx > ? AND step_type = ?`,
		idx, cortexStepTypePlannerResponse,
	).Scan(&laterPlanner); err == nil && laterPlanner.Valid {
		return nil
	}
	raw := errDetails
	if len(raw) == 0 {
		raw = stepPayload
	}
	return classifyErrorStepPayload(raw)
}

func classifyErrorStepPayload(raw []byte) *acp.PromptOutcomeOverride {
	text := extractReadableErrorText(raw)
	if text == "" {
		text = "Antigravity reported an upstream API error"
	}

	switch {
	case reQuotaExhausted.MatchString(text) || reResourceExhausted.MatchString(text):
		return &acp.PromptOutcomeOverride{
			ErrorMessage: text,
			Failure: &provider.FailureMeta{
				Class:    provider.FailureTransient,
				Boundary: provider.FailureBoundaryTurn,
				Reason:   provider.FailureReasonUsageLimit,
				Code:     "rate_limit",
			},
		}
	case reInvalidThinkingSig.MatchString(text):
		return &acp.PromptOutcomeOverride{
			ErrorMessage: text,
			Failure: &provider.FailureMeta{
				Class:    provider.FailureFatal,
				Boundary: provider.FailureBoundaryTurn,
				Code:     "invalid_thinking_signature",
			},
		}
	case reUpstreamServerErr.MatchString(text):
		return &acp.PromptOutcomeOverride{
			ErrorMessage: text,
			Failure: &provider.FailureMeta{
				Class:    provider.FailureTransientAfterRetry,
				Boundary: provider.FailureBoundaryTurn,
				Code:     "upstream_error",
			},
		}
	case reMidTurnNotFound.MatchString(text):
		return &acp.PromptOutcomeOverride{
			ErrorMessage: text,
			Failure: &provider.FailureMeta{
				Class:    provider.FailureFatal,
				Boundary: provider.FailureBoundaryTurn,
				Code:     "not_found",
			},
		}
	default:
		return &acp.PromptOutcomeOverride{
			ErrorMessage: text,
			Failure: &provider.FailureMeta{
				Class:    provider.FailureTransient,
				Boundary: provider.FailureBoundaryTurn,
				Code:     "provider_error",
			},
		}
	}
}

// extractReadableErrorText pulls printable UTF-8/ASCII segments from either
// protobuf-encoded or JSON/text step payloads, returning a bounded summary.
func extractReadableErrorText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) > maxErrorPayloadScanBytes {
		raw = raw[:maxErrorPayloadScanBytes]
	}
	matches := reHumanReadableASCII.FindAllString(string(raw), -1)
	if len(matches) == 0 {
		return ""
	}
	// Prefer a segment mentioning a known error code or sentence.
	var best string
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if reQuotaExhausted.MatchString(m) || reResourceExhausted.MatchString(m) ||
			reInvalidThinkingSig.MatchString(m) || reUpstreamServerErr.MatchString(m) ||
			strings.Contains(strings.ToLower(m), "error") {
			best = m
			break
		}
		if len(m) > len(best) {
			best = m
		}
	}
	if len(best) > 320 {
		best = best[:317] + "..."
	}
	return best
}
