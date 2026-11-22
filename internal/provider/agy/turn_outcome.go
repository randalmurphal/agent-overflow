package agy

import (
	"database/sql"
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
const cortexStepTypeErrorMessage = 17

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

// readMaxStepIndex returns the highest `idx` in `steps` before a prompt starts,
// or (0, false) if the conversation DB does not exist yet or cannot be read.
func readMaxStepIndex(appDataDir, sessionID string) (int, bool) {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return 0, false
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return 0, false
	}
	defer db.Close()

	var maxIdx sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
		return 0, false
	}
	if !maxIdx.Valid {
		return -1, true
	}
	return int(maxIdx.Int64), true
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
	defer db.Close()

	rows, err := db.Query(
		`SELECT idx, step_type, COALESCE(error_details, X''), COALESCE(step_payload, X'') FROM steps WHERE idx > ? AND step_type = ? ORDER BY idx DESC LIMIT 1`,
		sinceIdx, cortexStepTypeErrorMessage,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	if !rows.Next() {
		return nil
	}
	var idx, stepType int
	var errDetails, stepPayload []byte
	if err := rows.Scan(&idx, &stepType, &errDetails, &stepPayload); err != nil {
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
	matches := reHumanReadableASCII.FindAllString(string(raw), 16)
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
