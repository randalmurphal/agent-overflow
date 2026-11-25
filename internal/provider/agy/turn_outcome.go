package agy

import (
	"bytes"
	"context"
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
	cortexStepStatusRunning       = 2
	cortexStepStatusDone          = 3
	cortexStepStatusCanceled      = 6
	cortexStepStatusError         = 7
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
	return sql.Open("sqlite", sqliteFileDSN(path, "mode=ro&_pragma=busy_timeout(2000)"))
}

// readMaxStepIndex returns the highest `idx` in `steps` before a prompt starts.
// When the conversation DB does not exist yet (brand-new session before Turn 1),
// it returns (-1, true) so Turn 1 step_type=17 errors at idx >= 0 are still caught.
func readMaxStepIndex(ctx context.Context, appDataDir, sessionID string) (int, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	if err := db.QueryRowContext(ctx, `SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
		return 0, false
	}
	if !maxIdx.Valid {
		return -1, true
	}
	return int(maxIdx.Int64), true
}

// readMaxCheckpointIndex returns the initial compaction scan cursor as the
// current highest `idx` in `steps` (O(1) primary-key seek rather than a
// full-table scan over `step_type`), or -1 if the DB does not exist yet.
// Any compaction occurring after session open will land at `idx > cursor`.
func readMaxCheckpointIndex(ctx context.Context, appDataDir, sessionID string) int {
	maxIdx, ok := readMaxStepIndex(ctx, appDataDir, sessionID)
	if !ok {
		return -1
	}
	return maxIdx
}

// detectNewCheckpoint checks whether agy wrote a compaction CHECKPOINT step
// (step_type = 23) with idx > afterIdx. It always advances nextIdx to the
// current MAX(idx) (O(1) PK seek) so scanned non-checkpoint steps are never
// rescanned on subsequent turns, and skips opening SQLite when the DB+WAL stat
// signature has not changed since the last clean check.
func (f *Flavor) detectNewCheckpoint(ctx context.Context, sessionID string, afterIdx int) (int, *acp.CompactionEvent) {
	if ctx == nil {
		ctx = context.Background()
	}
	dbPath, err := conversationDBPath(f.AppDataDir, sessionID)
	if err != nil {
		return afterIdx, nil
	}
	sig, sigOK := statConversationDB(dbPath)
	if !sigOK {
		return afterIdx, nil
	}
	if f.compactionCheckCacheHit(sessionID, sig, afterIdx) {
		return afterIdx, nil
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return afterIdx, nil
	}
	defer func() { _ = db.Close() }()

	var maxAll sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(idx) FROM steps`).Scan(&maxAll); err != nil || !maxAll.Valid {
		return afterIdx, nil
	}
	highIdx := int(maxAll.Int64)
	if highIdx <= afterIdx {
		f.recordCompactionCheckMiss(sessionID, sig, afterIdx)
		return afterIdx, nil
	}

	var cpIdx sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(idx) FROM steps WHERE step_type = ? AND idx > ?`,
		cortexStepTypeCheckpoint, afterIdx,
	).Scan(&cpIdx); err != nil || !cpIdx.Valid {
		f.recordCompactionCheckMiss(sessionID, sig, highIdx)
		return highIdx, nil
	}
	newCP := int(cpIdx.Int64)
	nextCursor := highIdx
	if newCP > nextCursor {
		nextCursor = newCP
	}
	f.recordCompactionCheckMiss(sessionID, sig, nextCursor)
	return nextCursor, &acp.CompactionEvent{
		ItemID: fmt.Sprintf("agy-compact-%d", newCP),
	}
}

// inspectTurnErrorStep checks whether agy wrote a CORTEX_STEP_TYPE_ERROR_MESSAGE
// (step_type = 17) row with idx > sinceIdx during the prompt that just ended.
func inspectTurnErrorStep(ctx context.Context, appDataDir, sessionID string, sinceIdx int) *acp.PromptOutcomeOverride {
	if ctx == nil {
		ctx = context.Background()
	}
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return nil
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx,
		`SELECT idx, step_type, COALESCE(error_details, X''), COALESCE(step_payload, X'') FROM steps WHERE idx > ? AND step_type = ? ORDER BY idx DESC LIMIT 1`,
		sinceIdx, cortexStepTypeErrorMessage,
	)
	if err != nil {
		return nil
	}
	if !rows.Next() {
		_ = rows.Close()
		return nil
	}
	var idx, stepType int
	var errDetails, stepPayload []byte
	scanErr := rows.Scan(&idx, &stepType, &errDetails, &stepPayload)
	_ = rows.Close()
	if scanErr != nil {
		return nil
	}
	// If agy retried and succeeded with a subsequent planner response after
	// this error step, do not mark the whole turn failed.
	var laterPlanner sql.NullInt64
	if err := db.QueryRowContext(ctx,
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

// isProtobufMetadataToken reports whether s looks like an internal protobuf
// type URL or package identifier (for example "google.rpc.ErrorInfo" or
// "type.googleapis.com/google.rpc.ErrorInfo") rather than a human-readable
// error message.
func isProtobufMetadataToken(s string) bool {
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(s, "type.googleapis.com/") || strings.HasPrefix(s, "google.rpc.") {
		return true
	}
	return strings.Contains(s, ".") && (strings.Contains(s, "/") || strings.Count(s, ".") >= 2)
}

type dbFileSignature struct {
	dbSize     int64
	dbModNano  int64
	walExists  bool
	walSize    int64
	walModNano int64
}

type bgLookupSnapshot struct {
	sig        dbFileSignature
	checkedIDs map[string]struct{}
}

func statConversationDB(dbPath string) (dbFileSignature, bool) {
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return dbFileSignature{}, false
	}
	sig := dbFileSignature{
		dbSize:    dbInfo.Size(),
		dbModNano: dbInfo.ModTime().UnixNano(),
	}
	if walInfo, err := os.Stat(dbPath + "-wal"); err == nil {
		sig.walExists = true
		sig.walSize = walInfo.Size()
		sig.walModNano = walInfo.ModTime().UnixNano()
	}
	return sig, true
}

func (f *Flavor) bgLookupCacheHit(sessionID string, sig dbFileSignature, toolCallIDs []string) bool {
	f.bgLookupMu.Lock()
	defer f.bgLookupMu.Unlock()
	snap, ok := f.bgLookupCache[sessionID]
	if !ok || snap.sig != sig {
		return false
	}
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		if _, checked := snap.checkedIDs[id]; !checked {
			return false
		}
	}
	return true
}

func (f *Flavor) recordBgLookupMiss(sessionID string, sig dbFileSignature, toolCallIDs []string) {
	f.bgLookupMu.Lock()
	defer f.bgLookupMu.Unlock()
	if f.bgLookupCache == nil || len(f.bgLookupCache) >= 64 {
		f.bgLookupCache = make(map[string]bgLookupSnapshot)
	}
	snap, ok := f.bgLookupCache[sessionID]
	if !ok || snap.sig != sig {
		snap = bgLookupSnapshot{
			sig:        sig,
			checkedIDs: make(map[string]struct{}, len(toolCallIDs)),
		}
	}
	for _, id := range toolCallIDs {
		if id != "" {
			snap.checkedIDs[id] = struct{}{}
		}
	}
	f.bgLookupCache[sessionID] = snap
}

func (f *Flavor) clearBgLookupCache(sessionID string) {
	f.bgLookupMu.Lock()
	delete(f.bgLookupCache, sessionID)
	f.bgLookupMu.Unlock()
}

type compactionCheckSnapshot struct {
	sig      dbFileSignature
	afterIdx int
}

func (f *Flavor) compactionCheckCacheHit(sessionID string, sig dbFileSignature, afterIdx int) bool {
	f.compactionMu.Lock()
	defer f.compactionMu.Unlock()
	snap, ok := f.compactionCache[sessionID]
	return ok && snap.sig == sig && snap.afterIdx >= afterIdx
}

func (f *Flavor) recordCompactionCheckMiss(sessionID string, sig dbFileSignature, afterIdx int) {
	f.compactionMu.Lock()
	defer f.compactionMu.Unlock()
	if f.compactionCache == nil || len(f.compactionCache) >= 64 {
		f.compactionCache = make(map[string]compactionCheckSnapshot)
	}
	f.compactionCache[sessionID] = compactionCheckSnapshot{sig: sig, afterIdx: afterIdx}
}

// CanLookupBackgroundCommands reports whether this flavor can inspect agy's
// conversation DB for run_command steps that transitioned to background tasks.
func (f *Flavor) CanLookupBackgroundCommands(sessionID string) bool {
	return strings.TrimSpace(f.AppDataDir) != "" && validateConversationSessionID(sessionID) == nil
}

// LookupBackgroundCommands checks conversations/<sessionID>.db for any of
// toolCallIDs whose step has task_details populated (agy's marker for a
// run_command that exceeded WaitMsBeforeAsync or launched as a daemon).
// agy 1.5.16's ACP server omits toolOutput on in_progress tool_call_updates, so
// task_details is the authoritative signal for background promotion and log
// file correlation. minStepIdx bounds the scan via steps' PRIMARY KEY (idx) so
// multi-thousand-turn conversation DBs never suffer full-table scans.
func (f *Flavor) LookupBackgroundCommands(ctx context.Context, sessionID string, toolCallIDs []string, includeCompleted bool, minStepIdx int) map[string]acp.BackgroundCommandInfo {
	if !f.CanLookupBackgroundCommands(sessionID) || len(toolCallIDs) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dbPath, err := conversationDBPath(f.AppDataDir, sessionID)
	if err != nil {
		return nil
	}
	sig, sigOK := statConversationDB(dbPath)
	if !sigOK {
		return nil
	}
	if !includeCompleted && f.bgLookupCacheHit(sessionID, sig, toolCallIDs) {
		return nil
	}
	if f.bgLookupOpenHook != nil {
		f.bgLookupOpenHook()
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	if minStepIdx < -1 {
		minStepIdx = -1
	}
	var rows *sql.Rows
	if includeCompleted {
		rows, err = db.QueryContext(ctx,
			`SELECT idx, COALESCE(metadata, X''), task_details FROM steps WHERE idx > ? AND status IN (?, ?, ?, ?) AND task_details IS NOT NULL AND length(task_details) > 0 ORDER BY idx DESC`,
			minStepIdx, cortexStepStatusRunning, cortexStepStatusDone, cortexStepStatusCanceled, cortexStepStatusError,
		)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT idx, COALESCE(metadata, X''), task_details FROM steps WHERE idx > ? AND status = ? AND task_details IS NOT NULL AND length(task_details) > 0 ORDER BY idx DESC`,
			minStepIdx, cortexStepStatusRunning,
		)
	}
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	remaining := make(map[string]struct{}, len(toolCallIDs))
	for _, id := range toolCallIDs {
		if id != "" {
			remaining[id] = struct{}{}
		}
	}
	if len(remaining) == 0 {
		return nil
	}

	found := make(map[string]acp.BackgroundCommandInfo)
	for rows.Next() && len(remaining) > 0 {
		var idx int
		var metaBlob, taskBlob []byte
		if err := rows.Scan(&idx, &metaBlob, &taskBlob); err != nil {
			continue
		}
		matchedID := ""
		for id := range remaining {
			if containsToolCallIDToken(metaBlob, id) {
				matchedID = id
				break
			}
		}
		if matchedID == "" {
			continue
		}
		info := parseTaskDetailsBlob(f.AppDataDir, sessionID, idx, taskBlob)
		found[matchedID] = info
		delete(remaining, matchedID)
	}
	if len(found) == 0 {
		if !includeCompleted {
			f.recordBgLookupMiss(sessionID, sig, toolCallIDs)
		}
		return nil
	}
	// Only invalidate the negative cache if at least one found command still
	// carries a new log path (or keep misses cached for unchanged DB+WAL).
	hasOutputFile := false
	for _, info := range found {
		if info.OutputFile != "" {
			hasOutputFile = true
			break
		}
	}
	if hasOutputFile {
		f.clearBgLookupCache(sessionID)
	}
	return found
}

// containsToolCallIDToken reports whether callID appears in a protobuf metadata
// blob bounded by non-identifier bytes (so "call_123" never matches "call_1234").
func containsToolCallIDToken(blob []byte, callID string) bool {
	if len(blob) == 0 || callID == "" {
		return false
	}
	needle := []byte(callID)
	offset := 0
	for {
		pos := bytes.Index(blob[offset:], needle)
		if pos < 0 {
			return false
		}
		start := offset + pos
		end := start + len(needle)
		leftOK := start == 0 || !isCallIDByte(blob[start-1])
		rightOK := end == len(blob) || !isCallIDByte(blob[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
}

func isCallIDByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

func parseTaskDetailsBlob(appDataDir, sessionID string, stepIdx int, taskBlob []byte) acp.BackgroundCommandInfo {
	info := acp.BackgroundCommandInfo{
		TaskID: fmt.Sprintf("%s/task-%d", sessionID, stepIdx),
	}
	if len(taskBlob) > 0 {
		if rawURI := extractFileURI(taskBlob); rawURI != "" {
			if resolved := resolveTranscriptURI(rawURI, ""); resolved != "" && filepath.IsAbs(resolved) {
				info.OutputFile = filepath.Clean(resolved)
			}
		}
	}
	if info.OutputFile == "" && appDataDir != "" {
		candidate := filepath.Join(appDataDir, "brain", sessionID, ".system_generated", "tasks", fmt.Sprintf("task-%d.log", stepIdx))
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			info.OutputFile = candidate
		}
	}
	return info
}

func extractFileURI(blob []byte) string {
	const prefix = "file://"
	idx := bytes.Index(blob, []byte(prefix))
	if idx < 0 {
		return ""
	}
	end := idx + len(prefix)
	for end < len(blob) {
		b := blob[end]
		if b < 0x20 || b >= 0x7f || b == '"' || b == '\'' || b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			break
		}
		end++
	}
	if end <= idx+len(prefix) {
		return ""
	}
	return string(blob[idx:end])
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
	// Prefer the most descriptive segment mentioning a known error code or
	// sentence, skipping bare protobuf type URLs like google.rpc.ErrorInfo.
	var bestCode, bestSentence, bestFallback string
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if m == "" || isProtobufMetadataToken(m) {
			continue
		}
		if reQuotaExhausted.MatchString(m) || reResourceExhausted.MatchString(m) ||
			reInvalidThinkingSig.MatchString(m) || reUpstreamServerErr.MatchString(m) ||
			reMidTurnNotFound.MatchString(m) {
			if len(m) > len(bestCode) {
				bestCode = m
			}
			continue
		}
		if strings.Contains(strings.ToLower(m), "error") && len(m) > len(bestSentence) {
			bestSentence = m
			continue
		}
		if len(m) > len(bestFallback) {
			bestFallback = m
		}
	}
	best := bestCode
	if best == "" {
		best = bestSentence
	}
	if best == "" {
		best = bestFallback
	}
	if len(best) > 320 {
		best = best[:317] + "..."
	}
	return best
}
