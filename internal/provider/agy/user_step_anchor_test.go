package agy

import (
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendProtoVarintField(dst []byte, fieldNum uint64, val uint64) []byte {
	dst = binary.AppendUvarint(dst, (fieldNum<<3)|uint64(protoWireVarint))
	return binary.AppendUvarint(dst, val)
}

func appendProtoBytesField(dst []byte, fieldNum uint64, data []byte) []byte {
	dst = binary.AppendUvarint(dst, (fieldNum<<3)|uint64(protoWireBytes))
	dst = binary.AppendUvarint(dst, uint64(len(data)))
	return append(dst, data...)
}

func encodeTestStepMetadata(createdAtMs int64, stepHash string) []byte {
	var meta []byte
	if createdAtMs > 0 {
		var ts []byte
		ts = appendProtoVarintField(ts, 1, uint64(createdAtMs/1000))
		ts = appendProtoVarintField(ts, 2, uint64((createdAtMs%1000)*1_000_000))
		meta = appendProtoBytesField(meta, 1, ts)
	}
	if stepHash != "" {
		meta = appendProtoBytesField(meta, 35, []byte(stepHash))
	}
	return meta
}

// encodeTestUserInputStep encodes an agy 1.5.16 CORTEX_STEP_TYPE_USER_INPUT
// step_payload protobuf. When legacyChunksOnly is true, text is encoded only
// via repeated f19.f3[*].f1 (matching older status=5 rows without f19.f2).
func encodeTestUserInputStep(text, stepHash string, createdAtMs int64, hasMedia, legacyChunksOnly bool) []byte {
	var pl []byte
	pl = appendProtoVarintField(pl, 1, cortexStepTypeUserInput)
	if legacyChunksOnly {
		pl = appendProtoVarintField(pl, 4, 5)
	} else {
		pl = appendProtoVarintField(pl, 4, 3)
	}
	if meta := encodeTestStepMetadata(createdAtMs, stepHash); len(meta) > 0 {
		pl = appendProtoBytesField(pl, 5, meta)
	}
	var ui []byte
	if text != "" {
		if !legacyChunksOnly {
			ui = appendProtoBytesField(ui, 2, []byte(text))
		}
		chunk := appendProtoBytesField(nil, 1, []byte(text))
		ui = appendProtoBytesField(ui, 3, chunk)
	}
	if hasMedia {
		media := appendProtoBytesField(nil, 1, []byte("image/jpeg"))
		media = appendProtoBytesField(media, 5, []byte("/tmp/img.jpg"))
		ui = appendProtoBytesField(ui, 9, media)
	}
	if len(ui) > 0 {
		pl = appendProtoBytesField(pl, 19, ui)
	}
	return pl
}

func TestDecodeUserInputStepAndAnchorRoundTrip(t *testing.T) {
	t.Parallel()

	anchor := FormatStepAnchor(8428, "8873ac7c582f494d9896cf0e640961ab")
	if anchor != "agy-step:8428:8873ac7c582f494d9896cf0e640961ab" {
		t.Fatalf("FormatStepAnchor = %q", anchor)
	}
	idx, hash, ok := ParseStepAnchor(anchor)
	if !ok || idx != 8428 || hash != "8873ac7c582f494d9896cf0e640961ab" {
		t.Fatalf("ParseStepAnchor(%q) = (%d, %q, %v)", anchor, idx, hash, ok)
	}
	idxOnly, hashOnly, ok := ParseStepAnchor("agy-step:42")
	if !ok || idxOnly != 42 || hashOnly != "" {
		t.Fatalf("ParseStepAnchor(idx-only) = (%d, %q, %v)", idxOnly, hashOnly, ok)
	}
	for _, bad := range []string{"", "uuid-123", "agy-step:", "agy-step:-1", "agy-step:abc", "agy-step:10:"} {
		if _, _, ok := ParseStepAnchor(bad); ok {
			t.Fatalf("ParseStepAnchor(%q) unexpectedly succeeded", bad)
		}
	}

	tsMs := int64(1795563664500)
	livePayload := encodeTestUserInputStep("Ok now go", "73d1f5ba80b14a34bbe55789fc5132ab", tsMs, false, false)
	live := decodeUserInputStep(8427, livePayload, nil)
	if live.Idx != 8427 || live.Text != "Ok now go" || live.StepHash != "73d1f5ba80b14a34bbe55789fc5132ab" || live.CreatedAtMs != tsMs || live.HasMedia {
		t.Fatalf("decodeUserInputStep(live) = %+v", live)
	}

	legacyPayload := encodeTestUserInputStep("older prompt via chunks", "", 0, false, true)
	colMeta := encodeTestStepMetadata(tsMs-5000, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	legacy := decodeUserInputStep(9, legacyPayload, colMeta)
	if legacy.Text != "older prompt via chunks" || legacy.StepHash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || legacy.CreatedAtMs != tsMs-5000 {
		t.Fatalf("decodeUserInputStep(legacy+colMeta) = %+v", legacy)
	}

	mediaPayload := encodeTestUserInputStep("", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", tsMs, true, false)
	media := decodeUserInputStep(7980, mediaPayload, nil)
	if media.Text != "" || !media.HasMedia || media.StepHash != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("decodeUserInputStep(media-only) = %+v", media)
	}
}

func TestResolvePromptStepAnchorMonotonicForRepeatedSteers(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sid := "sess-anchor-1"
	dbPath := seedRichConversationDB(t, appDataDir, sid, []testStepRow{
		{idx: 0, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("first", "hash0", 1000, false, false)},
		{idx: 1, stepType: cortexStepTypePlannerResponse, payload: []byte("reply0")},
	})

	f := NewFlavor(appDataDir)
	beforeToken := f.BeforePrompt(context.Background(), sid)
	if beforeToken != 1 {
		t.Fatalf("BeforePrompt = %v, want 1", beforeToken)
	}

	// Simulate two identical steered prompts ("ok") arriving with the same beforeToken.
	appendTestStepRows(t, dbPath, []testStepRow{
		{idx: 2, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("ok", "hash2", 2000, false, false)},
		{idx: 3, stepType: cortexStepTypePlannerResponse, payload: []byte("reply2")},
		{idx: 4, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("ok", "hash4", 3000, false, false)},
		{idx: 5, stepType: cortexStepTypePlannerResponse, payload: []byte("reply4")},
	})

	a1 := f.ResolvePromptStepAnchor(context.Background(), sid, "ok", beforeToken)
	if a1 != "agy-step:2:hash2" {
		t.Fatalf("first ResolvePromptStepAnchor = %q, want agy-step:2:hash2", a1)
	}
	a2 := f.ResolvePromptStepAnchor(context.Background(), sid, "ok", beforeToken)
	if a2 != "agy-step:4:hash4" {
		t.Fatalf("second ResolvePromptStepAnchor = %q, want agy-step:4:hash4", a2)
	}

	// Single new media-only step after last resolved idx resolves unambiguously.
	appendTestStepRows(t, dbPath, []testStepRow{
		{idx: 6, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("", "hash6", 4000, true, false)},
	})
	a3 := f.ResolvePromptStepAnchor(context.Background(), sid, "", 5)
	if a3 != "agy-step:6:hash6" {
		t.Fatalf("media-only ResolvePromptStepAnchor = %q, want agy-step:6:hash6", a3)
	}

	// Nil beforeToken must never guess from step 0.
	if got := f.ResolvePromptStepAnchor(context.Background(), sid, "first", nil); got != "" {
		t.Fatalf("ResolvePromptStepAnchor(nil beforeToken) = %q, want empty", got)
	}
}

func TestForkConversationBeforeTargetStoredAnchorContracts(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	srcID := "sess-anchor-contracts"
	baseMs := time.Now().UnixMilli()

	// Seed 7 extra historical user steps before AO's first visible prompt,
	// plus duplicate "Ok restarted" prompts and a final target prompt.
	rows := make([]testStepRow, 0, 30)
	idx := 0
	for i := 0; i < 7; i++ {
		rows = append(rows,
			testStepRow{idx: idx, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("historical pre-AO prompt", "hist", baseMs+int64(i*1000), false, true)},
			testStepRow{idx: idx + 1, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		)
		idx += 2
	}
	rows = append(rows,
		testStepRow{idx: idx, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok restarted", "h-restart-1", baseMs+10_000, false, false)},
		testStepRow{idx: idx + 1, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 2, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok now go", "h-go", baseMs+20_000, false, false)},
		testStepRow{idx: idx + 3, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 4, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok restarted", "h-restart-2", baseMs+30_000, false, false)},
		testStepRow{idx: idx + 5, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
	)
	targetStepIdx := idx + 4 // step 18
	seedRichConversationDB(t, appDataDir, srcID, rows)

	// 1. Direct stored anchor slices at targetStepIdx - 1 regardless of 7-step drift and duplicate text.
	dstID, cleanup, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Anchor:        FormatStepAnchor(targetStepIdx, "h-restart-2"),
		Target:        UserPromptHint{Text: "Ok restarted"},
		PromptsBefore: 2,
		PromptsAfter:  0,
	})
	if err != nil {
		t.Fatalf("ForkConversationBeforeTarget(exact anchor): %v", err)
	}
	defer func() { _ = cleanup() }()
	if gotMax := readTestDBMaxIdx(t, appDataDir, dstID); gotMax != targetStepIdx-1 {
		t.Fatalf("sliced DB max idx = %d, want %d", gotMax, targetStepIdx-1)
	}

	// 2. Retry against the already-sliced DB (maxIdx < anchorIdx) succeeds and keeps existing prefix.
	retryID, retryCleanup, err := ForkConversationBeforeTarget(appDataDir, dstID, UserPromptCutTarget{
		Anchor:        FormatStepAnchor(targetStepIdx, "h-restart-2"),
		Target:        UserPromptHint{Text: "Ok restarted"},
		PromptsBefore: 2,
		PromptsAfter:  0,
	})
	if err != nil {
		t.Fatalf("ForkConversationBeforeTarget(already-cut retry): %v", err)
	}
	defer func() { _ = retryCleanup() }()
	if gotMax := readTestDBMaxIdx(t, appDataDir, retryID); gotMax != targetStepIdx-1 {
		t.Fatalf("already-cut retry DB max idx = %d, want %d", gotMax, targetStepIdx-1)
	}

	// 3. Hash mismatch refuses loudly.
	if _, _, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Anchor:        FormatStepAnchor(targetStepIdx, "wrong-hash"),
		Target:        UserPromptHint{Text: "Ok restarted"},
		PromptsBefore: 2,
		PromptsAfter:  0,
	}); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash mismatch error, got %v", err)
	}

	// 4. Missing anchor when maxStepIdx >= anchorIdx refuses loudly instead of guessing.
	if _, _, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Anchor:        FormatStepAnchor(targetStepIdx-1, "h-missing"), // planner response idx, not user step
		Target:        UserPromptHint{Text: "Ok restarted"},
		PromptsBefore: 2,
		PromptsAfter:  0,
	}); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("expected missing anchor refusal, got %v", err)
	}

	// 5. Malformed anchor refuses loudly.
	if _, _, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Anchor:        "agy-step:not-a-number",
		Target:        UserPromptHint{Text: "Ok restarted"},
		PromptsBefore: 2,
		PromptsAfter:  0,
	}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed anchor refusal, got %v", err)
	}
}

func TestForkConversationBeforeTargetContentNeighborAndTimestampDrift(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	srcID := "sess-live-drift-repro"
	t0 := int64(1795560000000)

	// Reproduces live bug: 7 extra historical user steps in agy DB before AO's
	// timeline, repeated "Ok restarted" messages, an image-only message, and
	// the reverted tail message without stored anchors.
	rows := []testStepRow{}
	idx := 0
	for i := 0; i < 7; i++ {
		rows = append(rows,
			testStepRow{idx: idx, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("extra cli prompt", "eh", t0+int64(i*1000), false, true)},
			testStepRow{idx: idx + 1, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		)
		idx += 2
	}
	// Earlier "Ok restarted" 50 minutes before the recent one, with matching Prev "setup".
	rows = append(rows,
		testStepRow{idx: idx, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("setup", "h1", t0+10_000, false, false)},
		testStepRow{idx: idx + 1, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 2, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok restarted", "h2", t0+20_000, false, false)},
		testStepRow{idx: idx + 3, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 4, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("", "h-img", t0+2_900_000, true, false)},
		testStepRow{idx: idx + 5, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 6, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok now go", "h-go", t0+3_000_000, false, false)},
		testStepRow{idx: idx + 7, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 8, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok restarted", "h-restart-recent", t0+3_060_000, false, false)},
		testStepRow{idx: idx + 9, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		testStepRow{idx: idx + 10, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("Ok this is a forked thread, reverting back to this message", "h-fork", t0+3_120_000, false, false)},
		testStepRow{idx: idx + 11, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
	)
	imgIdx := idx + 4
	recentRestartIdx := idx + 8
	forkMsgIdx := idx + 10
	seedRichConversationDB(t, appDataDir, srcID, rows)

	// 1. Unique message text cuts at exact forkMsgIdx - 1 despite 7-step ordinal drift.
	dst1, clean1, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "Ok this is a forked thread, reverting back to this message", CreatedAtMs: t0 + 3_120_100},
		Prev:          &UserPromptHint{Text: "Ok restarted", CreatedAtMs: t0 + 3_060_000},
		PromptsBefore: 5,
		PromptsAfter:  0,
	})
	if err != nil {
		t.Fatalf("unique text cut: %v", err)
	}
	defer func() { _ = clean1() }()
	if got := readTestDBMaxIdx(t, appDataDir, dst1); got != forkMsgIdx-1 {
		t.Fatalf("unique text cut maxIdx = %d, want %d", got, forkMsgIdx-1)
	}

	// 2. Duplicate "Ok restarted": timestamp tier picks the recent one (150ms delta)
	// even if Prev accidentally matches the 50-minute-older duplicate.
	dst2, clean2, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "Ok restarted", CreatedAtMs: t0 + 3_060_150},
		Prev:          &UserPromptHint{Text: "setup"}, // intentionally matches the OLD duplicate's Prev
		PromptsBefore: 4,
		PromptsAfter:  1,
	})
	if err != nil {
		t.Fatalf("duplicate text timestamp-tier cut: %v", err)
	}
	defer func() { _ = clean2() }()
	if got := readTestDBMaxIdx(t, appDataDir, dst2); got != recentRestartIdx-1 {
		t.Fatalf("duplicate text cut maxIdx = %d, want %d (recent restart)", got, recentRestartIdx-1)
	}

	// 3. Media-only message matches HasMedia step accurately.
	dst3, clean3, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "", HasMedia: true, CreatedAtMs: t0 + 2_900_200},
		PromptsBefore: 2,
		PromptsAfter:  3,
	})
	if err != nil {
		t.Fatalf("media-only cut: %v", err)
	}
	defer func() { _ = clean3() }()
	if got := readTestDBMaxIdx(t, appDataDir, dst3); got != imgIdx-1 {
		t.Fatalf("media-only cut maxIdx = %d, want %d", got, imgIdx-1)
	}

	// 4. Unwritten tail prompt (never written to agy DB) keeps full DB cleanly.
	dst4, clean4, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "never reached agy", CreatedAtMs: t0 + 3_200_000},
		Prev:          &UserPromptHint{Text: "Ok this is a forked thread, reverting back to this message"},
		PromptsBefore: 6,
		PromptsAfter:  0,
	})
	if err != nil {
		t.Fatalf("unwritten tail cut: %v", err)
	}
	defer func() { _ = clean4() }()
	if got := readTestDBMaxIdx(t, appDataDir, dst4); got != forkMsgIdx+1 {
		t.Fatalf("unwritten tail cut maxIdx = %d, want %d (full DB)", got, forkMsgIdx+1)
	}

	// 5. Missing non-tail prompt MUST refuse loudly rather than guessing an ordinal.
	if _, _, err := ForkConversationBeforeTarget(appDataDir, srcID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "does not exist in db", CreatedAtMs: t0 + 2_500_000},
		PromptsBefore: 3,
		PromptsAfter:  2,
	}); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("expected refusal for missing non-tail prompt, got %v", err)
	}

	// 6. Completely ambiguous duplicate (no timestamps, no distinguishing neighbors, unknown PromptsAfter) refuses loudly.
	ambigID := "sess-ambig"
	seedRichConversationDB(t, appDataDir, ambigID, []testStepRow{
		{idx: 0, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("ping", "", 0, false, false)},
		{idx: 1, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
		{idx: 2, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("ping", "", 0, false, false)},
		{idx: 3, stepType: cortexStepTypePlannerResponse, payload: []byte("r")},
	})
	if _, _, err := ForkConversationBeforeTarget(appDataDir, ambigID, UserPromptCutTarget{
		Target:        UserPromptHint{Text: "ping"},
		PromptsBefore: 1,
		PromptsAfter:  -1,
	}); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("expected ambiguity refusal, got %v", err)
	}
}

type testStepRow struct {
	idx      int
	stepType int
	payload  []byte
	meta     []byte
}

func seedRichConversationDB(t *testing.T, appDataDir, sessionID string, rows []testStepRow) string {
	t.Helper()
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	stmts := []string{
		`CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT)`,
		`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB, metadata BLOB)`,
		`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`CREATE TABLE executor_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`INSERT INTO trajectory_meta (trajectory_id, cascade_id) VALUES ('orig-traj', '` + sessionID + `')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %s: %v", s, err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO steps (idx, step_type, step_payload, metadata) VALUES (?, ?, ?, ?)`, r.idx, r.stepType, r.payload, r.meta); err != nil {
			t.Fatalf("insert step %d: %v", r.idx, err)
		}
	}
	return dbPath
}

func appendTestStepRows(t *testing.T, dbPath string, rows []testStepRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO steps (idx, step_type, step_payload, metadata) VALUES (?, ?, ?, ?)`, r.idx, r.stepType, r.payload, r.meta); err != nil {
			t.Fatalf("append step %d: %v", r.idx, err)
		}
	}
}

func readTestDBMaxIdx(t *testing.T, appDataDir, sessionID string) int {
	t.Helper()
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var maxIdx sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
		t.Fatal(err)
	}
	if !maxIdx.Valid {
		return -1
	}
	return int(maxIdx.Int64)
}
