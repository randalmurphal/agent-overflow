package agy

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const stepAnchorPrefix = "agy-step:"

// FormatStepAnchor encodes a native Antigravity USER_INPUT step index and
// optional 32-hex step hash into an opaque anchor string stored in
// provider_parent_uuid on AO user_text rows and message_anchors.
func FormatStepAnchor(idx int, stepHash string) string {
	if idx < 0 {
		return ""
	}
	stepHash = strings.TrimSpace(stepHash)
	if stepHash == "" {
		return fmt.Sprintf("%s%d", stepAnchorPrefix, idx)
	}
	return fmt.Sprintf("%s%d:%s", stepAnchorPrefix, idx, stepHash)
}

// ParseStepAnchor decodes an anchor produced by FormatStepAnchor.
func ParseStepAnchor(raw string) (idx int, stepHash string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, stepAnchorPrefix) {
		return 0, "", false
	}
	rest := raw[len(stepAnchorPrefix):]
	if rest == "" {
		return 0, "", false
	}
	idxPart := rest
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		idxPart = rest[:colon]
		stepHash = strings.TrimSpace(rest[colon+1:])
		if stepHash == "" {
			return 0, "", false
		}
	}
	n, err := strconv.Atoi(idxPart)
	if err != nil || n < 0 {
		return 0, "", false
	}
	return n, stepHash, true
}

// UserPromptHint describes one AO native prompt row (user message or
// dispatched AskUserQuestion reply) for matching against agy's steps table
// when a stored native step anchor is absent.
type UserPromptHint struct {
	Text              string
	HasMedia          bool
	CreatedAtMs       int64
	IsQuestion        bool
	IsExpandedCommand bool
}

// UserPromptCutTarget identifies the native Antigravity USER_INPUT step to cut
// strictly before during fork or revert.
type UserPromptCutTarget struct {
	// Anchor is the stored native step anchor ("agy-step:<idx>:<hash>" or
	// "agy-step:<idx>") from item.Meta / message_anchors.provider_parent_uuid.
	Anchor string
	// Target describes the prompt being cut strictly before.
	Target UserPromptHint
	// Prev and Next are the immediately adjacent AO native prompts (when any),
	// used to disambiguate duplicate message texts on threads whose messages
	// predate native anchor stamping.
	Prev *UserPromptHint
	Next *UserPromptHint
	// PromptsBefore is the number of AO native prompts strictly before Target.
	PromptsBefore int
	// PromptsAfter is the number of AO native prompts strictly after Target
	// (-1 if unknown).
	PromptsAfter int
}

type nativeUserStep struct {
	Idx         int
	Text        string
	HasMedia    bool
	CreatedAtMs int64
	StepHash    string
}

type sqlQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func stepsHasMetadataColumn(q sqlQuerier) bool {
	rows, err := q.Query(`PRAGMA table_info(steps)`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk) == nil && name == "metadata" {
			return true
		}
	}
	return false
}

func readNativeUserSteps(q sqlQuerier, minExclusiveIdx int) ([]nativeUserStep, int, error) {
	hasMetaCol := stepsHasMetadataColumn(q)

	var maxIdxNull sql.NullInt64
	if err := q.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdxNull); err != nil {
		return nil, -1, fmt.Errorf("query max step idx: %w", err)
	}
	maxStepIdx := -1
	if maxIdxNull.Valid {
		maxStepIdx = int(maxIdxNull.Int64)
	}

	var rows *sql.Rows
	var err error
	if hasMetaCol {
		rows, err = q.Query(
			`SELECT idx, COALESCE(step_payload, X''), COALESCE(metadata, X'') FROM steps WHERE step_type = ? AND idx > ? ORDER BY idx ASC`,
			cortexStepTypeUserInput, minExclusiveIdx,
		)
	} else {
		rows, err = q.Query(
			`SELECT idx, COALESCE(step_payload, X'') FROM steps WHERE step_type = ? AND idx > ? ORDER BY idx ASC`,
			cortexStepTypeUserInput, minExclusiveIdx,
		)
	}
	if err != nil {
		return nil, maxStepIdx, fmt.Errorf("query user steps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []nativeUserStep
	for rows.Next() {
		var idx int
		var payload, metaBlob []byte
		if hasMetaCol {
			if scanErr := rows.Scan(&idx, &payload, &metaBlob); scanErr != nil {
				return nil, maxStepIdx, scanErr
			}
		} else {
			if scanErr := rows.Scan(&idx, &payload); scanErr != nil {
				return nil, maxStepIdx, scanErr
			}
		}
		step := decodeUserInputStep(idx, payload, metaBlob)
		out = append(out, step)
	}
	if err := rows.Err(); err != nil {
		return nil, maxStepIdx, err
	}
	return out, maxStepIdx, nil
}

// lookupUserPromptAnchor resolves the newly persisted CORTEX_STEP_TYPE_USER_INPUT
// step written by agy after minExclusiveIdx and returns its (idx, anchor).
// Returns (-1, "") when the conversation DB is unavailable or no unambiguous
// user step matches.
func lookupUserPromptAnchor(appDataDir, sessionID, content string, minExclusiveIdx int) (int, string) {
	if strings.TrimSpace(appDataDir) == "" || strings.TrimSpace(sessionID) == "" {
		return -1, ""
	}
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return -1, ""
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return -1, ""
	}
	defer func() { _ = db.Close() }()

	steps, _, err := readNativeUserSteps(db, minExclusiveIdx)
	if err != nil || len(steps) == 0 {
		return -1, ""
	}
	if strings.TrimSpace(content) != "" {
		for _, s := range steps {
			if promptTextMatchesStep(content, false, s) {
				return s.Idx, FormatStepAnchor(s.Idx, s.StepHash)
			}
		}
	}
	if len(steps) == 1 {
		// Single new user step since beforeToken: handles media-only prompts,
		// expanded composer slash commands, and output-schema-wrapped prompts
		// without guessing among multiple concurrent steps.
		return steps[0].Idx, FormatStepAnchor(steps[0].Idx, steps[0].StepHash)
	}
	return -1, ""
}

// resolveUserPromptCutIdx determines the exact step `idx` upper bound to keep
// inside a cloned conversation DB so the target user prompt and everything
// after it are removed. Returns CutKeepAll when the target was already cut or
// was never written at the tail of the conversation DB.
func resolveUserPromptCutIdx(tx *sql.Tx, target UserPromptCutTarget) (int, error) {
	userSteps, maxStepIdx, err := readNativeUserSteps(tx, -1)
	if err != nil {
		return 0, fmt.Errorf("agy fork: %w", err)
	}

	// 1. Authoritative stored native anchor ("agy-step:<idx>:<hash>").
	if rawAnchor := strings.TrimSpace(target.Anchor); rawAnchor != "" {
		anchorIdx, anchorHash, ok := ParseStepAnchor(rawAnchor)
		if !ok {
			return 0, fmt.Errorf("agy fork: malformed stored step anchor %q", rawAnchor)
		}
		for _, s := range userSteps {
			if s.Idx != anchorIdx {
				continue
			}
			if anchorHash != "" && s.StepHash != "" && s.StepHash != anchorHash {
				return 0, fmt.Errorf("agy fork: stored step anchor %q hash mismatch (db step %d has hash %q); refusing to cut wrong message", rawAnchor, s.Idx, s.StepHash)
			}
			if s.Idx <= 0 {
				return 0, errors.New("agy fork: cut before first step leaves no prefix")
			}
			return s.Idx - 1, nil
		}
		// Anchor step absent: if the DB already ends strictly before anchorIdx
		// (retry after a prior rollback sliced the DB, or provider stopped
		// before persisting this tail step), keep the existing prefix intact.
		if maxStepIdx < anchorIdx {
			return CutKeepAll, nil
		}
		return 0, fmt.Errorf("agy fork: stored step anchor %q not found in conversation DB (max step %d); refusing to guess cut position", rawAnchor, maxStepIdx)
	}

	// 2. Fallback for pre-existing/imported messages without a stored anchor:
	// match by exact content/media, disambiguating duplicates with neighbor
	// context, creation timestamp proximity, and tail distance.
	candidates := matchUserStepCandidates(userSteps, target)
	if len(candidates) == 0 {
		if isUnwrittenTailPrompt(userSteps, target) {
			return CutKeepAll, nil
		}
		return 0, fmt.Errorf("agy fork: target user message not found in conversation DB (%d recorded native prompts); refusing to guess cut position", len(userSteps))
	}

	bestPos, ambiguous := selectBestUserStepCandidate(userSteps, candidates, target)
	if ambiguous {
		return 0, fmt.Errorf("agy fork: target user message matched %d identical steps in conversation DB with no unique neighbor or timestamp match; refusing to guess cut position", len(candidates))
	}
	targetIdx := userSteps[bestPos].Idx
	if targetIdx <= 0 {
		return 0, errors.New("agy fork: cut before first step leaves no prefix")
	}
	return targetIdx - 1, nil
}

func matchUserStepCandidates(userSteps []nativeUserStep, target UserPromptCutTarget) []int {
	var exact []int
	for pos, s := range userSteps {
		if hintMatchesStepExact(target.Target, s) {
			exact = append(exact, pos)
		}
	}
	if len(exact) > 0 {
		return exact
	}

	// Secondary match for expanded slash commands or dispatched AskUserQuestion
	// replies whose wire prompt differs from item.Summary: require a tight
	// timestamp window AND at least one neighbor (or tail position) confirmation.
	if (target.Target.IsExpandedCommand || target.Target.IsQuestion) && target.Target.CreatedAtMs > 0 {
		const maxTimestampDeltaMs = int64(30_000)
		var byTime []int
		for pos, s := range userSteps {
			if s.CreatedAtMs <= 0 {
				continue
			}
			if absInt64(s.CreatedAtMs-target.Target.CreatedAtMs) > maxTimestampDeltaMs {
				continue
			}
			if neighborScore(userSteps, pos, target) > 0 || (target.PromptsAfter >= 0 && len(userSteps)-1-pos == target.PromptsAfter) {
				byTime = append(byTime, pos)
			}
		}
		if len(byTime) > 0 {
			return byTime
		}
	}
	return nil
}

func isUnwrittenTailPrompt(userSteps []nativeUserStep, target UserPromptCutTarget) bool {
	if target.PromptsAfter != 0 {
		return false
	}
	if len(userSteps) == 0 {
		return true
	}
	last := userSteps[len(userSteps)-1]
	if target.Prev != nil && hintMatchesStepAsNeighbor(*target.Prev, last) {
		return true
	}
	if target.Target.CreatedAtMs > 0 && last.CreatedAtMs > 0 && target.Target.CreatedAtMs >= last.CreatedAtMs {
		return true
	}
	return false
}

type candidateScore struct {
	pos            int
	neighbors      int
	hasTimestamp   bool
	timestampDelta int64
	hasTailOffset  bool
	tailDelta      int
}

func selectBestUserStepCandidate(userSteps []nativeUserStep, candidates []int, target UserPromptCutTarget) (bestPos int, ambiguous bool) {
	if len(candidates) == 1 {
		return candidates[0], false
	}
	scores := make([]candidateScore, 0, len(candidates))
	for _, pos := range candidates {
		s := userSteps[pos]
		sc := candidateScore{
			pos:       pos,
			neighbors: neighborScore(userSteps, pos, target),
		}
		if target.Target.CreatedAtMs > 0 && s.CreatedAtMs > 0 {
			sc.hasTimestamp = true
			sc.timestampDelta = absInt64(s.CreatedAtMs - target.Target.CreatedAtMs)
		}
		if target.PromptsAfter >= 0 {
			sc.hasTailOffset = true
			tailOffset := len(userSteps) - 1 - pos
			diff := tailOffset - target.PromptsAfter
			if diff < 0 {
				diff = -diff
			}
			sc.tailDelta = diff
		}
		scores = append(scores, sc)
	}

	best := scores[0]
	tied := false
	for _, sc := range scores[1:] {
		cmp := compareCandidateScores(sc, best)
		if cmp > 0 {
			best = sc
			tied = false
		} else if cmp == 0 {
			tied = true
		}
	}
	if tied {
		return 0, true
	}
	return best.pos, false
}

func timestampTier(sc candidateScore) int {
	if !sc.hasTimestamp {
		return 1
	}
	if sc.timestampDelta <= 15_000 {
		return 2
	}
	if sc.timestampDelta <= 120_000 {
		return 1
	}
	return 0
}

// compareCandidateScores returns >0 if a is strictly better than b, <0 if b is
// strictly better than a, or 0 if both are indistinguishable.
func compareCandidateScores(a, b candidateScore) int {
	if a.hasTimestamp && b.hasTimestamp {
		ta, tb := timestampTier(a), timestampTier(b)
		if ta != tb {
			if ta > tb {
				return 1
			}
			return -1
		}
	}
	if a.neighbors != b.neighbors {
		if a.neighbors > b.neighbors {
			return 1
		}
		return -1
	}
	if a.hasTimestamp && b.hasTimestamp && a.timestampDelta != b.timestampDelta {
		if a.timestampDelta < b.timestampDelta {
			return 1
		}
		return -1
	}
	if a.hasTailOffset && b.hasTailOffset && a.tailDelta != b.tailDelta {
		if a.tailDelta < b.tailDelta {
			return 1
		}
		return -1
	}
	return 0
}

func neighborScore(userSteps []nativeUserStep, pos int, target UserPromptCutTarget) int {
	score := 0
	if target.Prev != nil && pos > 0 && hintMatchesStepAsNeighbor(*target.Prev, userSteps[pos-1]) {
		score++
	}
	if target.Next != nil && pos+1 < len(userSteps) && hintMatchesStepAsNeighbor(*target.Next, userSteps[pos+1]) {
		score++
	}
	return score
}

var reImageMarker = regexp.MustCompile(`\[Image #\d+\]`)

func stripImageMarkers(s string) string {
	return strings.Join(strings.Fields(reImageMarker.ReplaceAllString(s, " ")), " ")
}

// promptTextMatchesStep compares an AO prompt string (which retains composer
// `[Image #N]` placeholders that buildPromptBlocks strips before sending to
// agy) against a decoded native USER_INPUT step.
func promptTextMatchesStep(rawPrompt string, hasMediaHint bool, step nativeUserStep) bool {
	want := normalizePromptText(rawPrompt)
	got := normalizePromptText(step.Text)
	if want != "" && got == want {
		return true
	}
	if !step.HasMedia && !hasMediaHint {
		return false
	}
	wantStripped := stripImageMarkers(want)
	gotStripped := stripImageMarkers(got)
	if wantStripped != "" {
		return step.HasMedia && wantStripped == gotStripped
	}
	return step.HasMedia && gotStripped == "" && (hasMediaHint || reImageMarker.MatchString(want))
}

func hintMatchesStepExact(hint UserPromptHint, step nativeUserStep) bool {
	if hint.IsQuestion {
		return false
	}
	return promptTextMatchesStep(hint.Text, hint.HasMedia, step)
}

func hintMatchesStepAsNeighbor(hint UserPromptHint, step nativeUserStep) bool {
	if hintMatchesStepExact(hint, step) {
		return true
	}
	if (hint.IsQuestion || hint.IsExpandedCommand) && hint.CreatedAtMs > 0 && step.CreatedAtMs > 0 {
		return absInt64(step.CreatedAtMs-hint.CreatedAtMs) <= 30_000
	}
	return false
}

func normalizePromptText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(s)
}

func absInt64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

// decodeUserInputStep decodes agy's CORTEX_STEP_TYPE_USER_INPUT protobuf row
// (`steps.step_payload` and optional `steps.metadata` column).
//
// Verified against live agy 1.5.16 conversations/<id>.db:
//   - step_payload.f5 (or steps.metadata): StepMetadata
//   - f1: google.protobuf.Timestamp (f1=seconds, f2=nanos)
//   - f35: 32-hex per-step hash
//   - step_payload.f19: UserInputStep
//   - f2: raw user text (present on live ACP prompts)
//   - f3 (repeated): TextOrScopeItem (f1=text chunk; present on all text prompts)
//   - f9 (repeated): Media attachment message
func decodeUserInputStep(idx int, stepPayload, metaColumn []byte) nativeUserStep {
	step := nativeUserStep{Idx: idx}
	var payloadMeta []byte
	var userInputMsg []byte

	walkProtoFields(stepPayload, func(fieldNum uint64, wireType byte, val uint64, data []byte) {
		if wireType != protoWireBytes {
			return
		}
		switch fieldNum {
		case 5:
			payloadMeta = data
		case 19:
			userInputMsg = data
		}
	})

	if len(payloadMeta) > 0 {
		step.CreatedAtMs, step.StepHash = decodeStepMetadata(payloadMeta)
	}
	if len(metaColumn) > 0 && (step.CreatedAtMs == 0 || step.StepHash == "") {
		colTime, colHash := decodeStepMetadata(metaColumn)
		if step.CreatedAtMs == 0 {
			step.CreatedAtMs = colTime
		}
		if step.StepHash == "" {
			step.StepHash = colHash
		}
	}
	if len(userInputMsg) > 0 {
		step.Text, step.HasMedia = decodeUserInputMessage(userInputMsg)
	}
	return step
}

func decodeStepMetadata(raw []byte) (createdAtMs int64, stepHash string) {
	walkProtoFields(raw, func(fieldNum uint64, wireType byte, val uint64, data []byte) {
		if wireType != protoWireBytes {
			return
		}
		switch fieldNum {
		case 1:
			if createdAtMs == 0 {
				createdAtMs = decodeProtoTimestampMs(data)
			}
		case 35:
			if stepHash == "" {
				stepHash = strings.TrimSpace(string(data))
			}
		}
	})
	return createdAtMs, stepHash
}

func decodeProtoTimestampMs(raw []byte) int64 {
	var seconds, nanos int64
	walkProtoFields(raw, func(fieldNum uint64, wireType byte, val uint64, _ []byte) {
		if wireType != protoWireVarint {
			return
		}
		switch fieldNum {
		case 1:
			seconds = int64(val)
		case 2:
			nanos = int64(val)
		}
	})
	if seconds <= 0 {
		return 0
	}
	return seconds*1000 + nanos/1_000_000
}

func decodeUserInputMessage(raw []byte) (text string, hasMedia bool) {
	var rawText string
	var chunks []string
	walkProtoFields(raw, func(fieldNum uint64, wireType byte, _ uint64, data []byte) {
		if wireType != protoWireBytes {
			return
		}
		switch fieldNum {
		case 2:
			if rawText == "" {
				rawText = string(data)
			}
		case 3:
			walkProtoFields(data, func(subField uint64, subWire byte, _ uint64, subData []byte) {
				if subField == 1 && subWire == protoWireBytes {
					chunks = append(chunks, string(subData))
				}
			})
		case 9:
			hasMedia = true
		}
	})
	if rawText != "" {
		return rawText, hasMedia
	}
	if len(chunks) > 0 {
		return strings.Join(chunks, ""), hasMedia
	}
	return "", hasMedia
}

const (
	protoWireVarint  byte = 0
	protoWireFixed64 byte = 1
	protoWireBytes   byte = 2
	protoWireFixed32 byte = 5
)

func walkProtoFields(buf []byte, fn func(fieldNum uint64, wireType byte, varint uint64, data []byte)) {
	i := 0
	for i < len(buf) {
		tag, n := binary.Uvarint(buf[i:])
		if n <= 0 {
			return
		}
		i += n
		fieldNum := tag >> 3
		wireType := byte(tag & 7)
		if fieldNum == 0 {
			return
		}
		switch wireType {
		case protoWireVarint:
			val, m := binary.Uvarint(buf[i:])
			if m <= 0 {
				return
			}
			i += m
			fn(fieldNum, wireType, val, nil)
		case protoWireFixed64:
			if i+8 > len(buf) {
				return
			}
			val := binary.LittleEndian.Uint64(buf[i : i+8])
			i += 8
			fn(fieldNum, wireType, val, nil)
		case protoWireBytes:
			length, m := binary.Uvarint(buf[i:])
			if m <= 0 {
				return
			}
			i += m
			if length > uint64(len(buf)-i) {
				return
			}
			end := i + int(length)
			fn(fieldNum, wireType, 0, buf[i:end])
			i = end
		case protoWireFixed32:
			if i+4 > len(buf) {
				return
			}
			val := uint64(binary.LittleEndian.Uint32(buf[i : i+4]))
			i += 4
			fn(fieldNum, wireType, val, nil)
		default:
			return
		}
	}
}
