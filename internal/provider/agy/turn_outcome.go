package agy

import (
	"database/sql"
	"fmt"
	"net/url"
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

func conversationDBPath(appDataDir, sessionID string) string {
	return filepath.Join(appDataDir, "conversations", sessionID+".db")
}

func openConversationDBReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(100)", url.PathEscape(path))
	// Preserve slashes in file URI paths.
	dsn = strings.ReplaceAll(dsn, "%2F", "/")
	return sql.Open("sqlite", dsn)
}

// readMaxStepIndex returns the highest `idx` in `steps` before a prompt starts,
// or (0, false) if the conversation DB does not exist yet or cannot be read.
func readMaxStepIndex(appDataDir, sessionID string) (int, bool) {
	db, err := openConversationDBReadOnly(conversationDBPath(appDataDir, sessionID))
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
	db, err := openConversationDBReadOnly(conversationDBPath(appDataDir, sessionID))
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT idx, step_type, data FROM steps WHERE idx > ? AND step_type = ? ORDER BY idx DESC LIMIT 1`,
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
	var raw []byte
	if err := rows.Scan(&idx, &stepType, &raw); err != nil {
		return nil
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
