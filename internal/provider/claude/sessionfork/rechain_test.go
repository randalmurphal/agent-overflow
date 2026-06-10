package sessionfork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apiErrorOffBranchFixture is the sanitized replica of the 2026-06-10
// incident topology, checked in as an authoritative wire sample. File
// order: u1 → a1 → u2-toolresult → a2 → a3-final, then two deferred
// system/api_error rows whose parentUuid points at u2-toolresult (the
// transcript leaf when the API retry fired — BYPASSING a2/a3-final),
// then u3 chained onto the api_errors.
const apiErrorOffBranchFixture = "../../../../docs/references/fixtures/claude/session_api_error_offbranch.jsonl"

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(data)
}

// parseForkedByOldUUID unmarshals forked output lines and indexes the
// transcript rows by their forkedFrom.messageUuid (the pre-fork uuid),
// so assertions can reference fixture row names despite the remint.
func parseForkedByOldUUID(t *testing.T, lines []string) map[string]map[string]any {
	t.Helper()
	rows := make(map[string]map[string]any, len(lines))
	for i, line := range lines {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		ff, _ := e["forkedFrom"].(map[string]any)
		oldID, _ := ff["messageUuid"].(string)
		if oldID != "" {
			rows[oldID] = e
		}
	}
	return rows
}

func forkedUUID(t *testing.T, rows map[string]map[string]any, oldID string) string {
	t.Helper()
	row, ok := rows[oldID]
	if !ok {
		t.Fatalf("forked output missing row for source uuid %q", oldID)
	}
	u, _ := row["uuid"].(string)
	return u
}

func forkedParent(t *testing.T, rows map[string]map[string]any, oldID string) string {
	t.Helper()
	row, ok := rows[oldID]
	if !ok {
		t.Fatalf("forked output missing row for source uuid %q", oldID)
	}
	p, _ := row["parentUuid"].(string)
	return p
}

// assertTailChainVisits walks parentUuid from the forked row for
// fromOldID and asserts every uuid in wantOldIDs is on that chain —
// i.e. the active branch from the file tail passes through every kept
// content row.
func assertTailChainVisits(t *testing.T, rows map[string]map[string]any, fromOldID string, wantOldIDs ...string) {
	t.Helper()
	parentByUUID := make(map[string]string, len(rows))
	for _, row := range rows {
		u, _ := row["uuid"].(string)
		p, _ := row["parentUuid"].(string)
		parentByUUID[u] = p
	}
	visited := make(map[string]bool, len(rows))
	cur := forkedUUID(t, rows, fromOldID)
	for cur != "" && !visited[cur] {
		visited[cur] = true
		cur = parentByUUID[cur]
	}
	for _, oldID := range wantOldIDs {
		if !visited[forkedUUID(t, rows, oldID)] {
			t.Errorf("active branch from %q does not visit %q — kept content is off-branch", fromOldID, oldID)
		}
	}
}

// TestBuildForkLines_RechainsOffBranchAPIErrorTail: a slice that keeps
// the deferred api_error rows as the file tail (the revert-fork shape
// from the incident) must re-chain them through the preceding content
// so the branch walk from the tail visits every kept row. Pre-fix, the
// forked file preserved err1.parentUuid → u2-toolresult, leaving
// a2/a3-final off-branch and resume-at hard-failing.
func TestBuildForkLines_RechainsOffBranchAPIErrorTail(t *testing.T) {
	src := readFixture(t, apiErrorOffBranchFixture)
	_, lines, uuidMap, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "err2", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)

	if got, want := forkedParent(t, rows, "err1"), forkedUUID(t, rows, "a3-final"); got != want {
		t.Errorf("err1 parent = %q, want re-chained to a3-final %q", got, want)
	}
	if got, want := forkedParent(t, rows, "err2"), forkedUUID(t, rows, "err1"); got != want {
		t.Errorf("err2 parent = %q, want err1 %q", got, want)
	}
	assertTailChainVisits(t, rows, "err2", "err1", "a3-final", "a2", "u2-toolresult", "a1", "u1")

	// The uuidMap covers every kept transcript row (7 of the 8 fixture
	// rows — u3 is past the slice).
	if len(uuidMap) != 7 {
		t.Errorf("uuidMap entries = %d, want 7", len(uuidMap))
	}
	if _, hasSliced := uuidMap["u3"]; hasSliced {
		t.Errorf("uuidMap contains u3, which the slice dropped")
	}
}

// TestBuildForkLines_AnchorParentIsAPIErrorRow exercises the exact
// incident entry point: WriteForkFileForUserMessageUUID anchored at a
// user row whose parentUuid IS a deferred api_error row. The written
// fork must have a linear tail with the timestamp bump landing on the
// (now on-branch) last writable row.
func TestBuildForkLines_AnchorParentIsAPIErrorRow(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(readFixture(t, apiErrorOffBranchFixture)), 0o600); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}

	newID, newPath, uuidMap, err := WriteForkFileForUserMessageUUID(srcPath, "u3", "")
	if err != nil {
		t.Fatalf("WriteForkFileForUserMessageUUID: %v", err)
	}
	if newID == "" || newPath == "" {
		t.Fatalf("empty fork output: id=%q path=%q", newID, newPath)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	rows := parseForkedByOldUUID(t, strings.Split(strings.TrimSpace(string(data)), "\n"))

	if got, want := forkedParent(t, rows, "err1"), forkedUUID(t, rows, "a3-final"); got != want {
		t.Errorf("err1 parent = %q, want a3-final %q", got, want)
	}
	assertTailChainVisits(t, rows, "err2", "a3-final", "a2", "u2-toolresult", "a1", "u1")

	// Timestamp bump lands on the last writable row (err2) — now
	// on-branch, so its documented purpose (leaf detection on resume)
	// holds. Earlier rows keep their authored timestamps.
	if ts, _ := rows["err2"]["timestamp"].(string); ts == "2026-06-10T05:45:00.444Z" {
		t.Errorf("last writable row's timestamp was not bumped")
	}
	if ts, _ := rows["a3-final"]["timestamp"].(string); ts != "2026-06-10T05:49:00.000Z" {
		t.Errorf("a3-final timestamp = %q, want authored time preserved", ts)
	}
	if _, ok := uuidMap["a3-final"]; !ok {
		t.Errorf("uuidMap missing kept content row a3-final")
	}
}

// TestBuildForkLines_RechainsMidFileAPIErrorsKeepsNextTurnChained: a
// full clone keeps u3 (chained onto err2 in the source). After
// re-chaining, the branch walk from u3 must pass through the
// previously-bypassed a3-final — the fork RESTORES the dropped tail to
// resumed context, matching the verified bisect repair.
func TestBuildForkLines_RechainsMidFileAPIErrorsKeepsNextTurnChained(t *testing.T) {
	src := readFixture(t, apiErrorOffBranchFixture)
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)

	if got, want := forkedParent(t, rows, "u3"), forkedUUID(t, rows, "err2"); got != want {
		t.Errorf("u3 parent = %q, want err2 %q (source chain preserved)", got, want)
	}
	assertTailChainVisits(t, rows, "u3", "err2", "err1", "a3-final", "a2", "u2-toolresult", "a1", "u1")
}

// TestBuildForkLines_CompactBoundarySystemRowNotRechained: compact
// boundary system rows are legitimate chain ROOTS (parentUuid null,
// summary user row chains onto them). The re-chain rule is scoped by
// subtype, so they must pass through untouched — root status preserved,
// logicalParentUuid still remapped.
func TestBuildForkLines_CompactBoundarySystemRowNotRechained(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"system","subtype":"compact_boundary","uuid":"sys-compact","parentUuid":null,"logicalParentUuid":"a1","sessionId":"src","content":"compaction event"}
{"type":"user","uuid":"cs1","parentUuid":"sys-compact","sessionId":"src","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued..."}}
{"type":"assistant","uuid":"a2","parentUuid":"cs1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"resumed"}]}}
`
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)

	if p, isStr := rows["sys-compact"]["parentUuid"].(string); isStr && p != "" {
		t.Errorf("compact boundary parent = %q, want preserved null root (re-chain rule leaked past its subtype scope)", p)
	}
	if got, want := rows["sys-compact"]["logicalParentUuid"], forkedUUID(t, rows, "a1"); got != want {
		t.Errorf("compact boundary logicalParentUuid = %v, want remapped a1 %q", got, want)
	}
	if got, want := forkedParent(t, rows, "cs1"), forkedUUID(t, rows, "sys-compact"); got != want {
		t.Errorf("compact summary parent = %q, want sys-compact %q", got, want)
	}
}

// TestBuildForkLines_OnBranchAPIErrorIsNoop: an api_error row claude
// wrote promptly (parent == file predecessor) re-chains to the same
// value — the forced-predecessor rule is a structural no-op there.
func TestBuildForkLines_OnBranchAPIErrorIsNoop(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"a1","sessionId":"src","level":"error","retryAttempt":1,"error":{"message":"Connection error."}}
{"type":"user","uuid":"u2","parentUuid":"err1","sessionId":"src","message":{"role":"user","content":"second"}}
`
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)
	if got, want := forkedParent(t, rows, "err1"), forkedUUID(t, rows, "a1"); got != want {
		t.Errorf("on-branch api_error parent = %q, want unchanged a1 %q", got, want)
	}
	if got, want := forkedParent(t, rows, "u2"), forkedUUID(t, rows, "err1"); got != want {
		t.Errorf("u2 parent = %q, want err1 %q", got, want)
	}
}

// TestBuildForkLines_FirstWritableAPIErrorNotRechained pins the i == 0
// guard: an api_error landing as the fork's FIRST writable row has no
// predecessor to chain to — forcing prevWritableNewUUID there would
// stamp an empty-string parent, which is junk (neither a valid uuid nor
// the null that marks a root). It keeps resolveParent's verdict
// instead: nil, because its stale parent is outside the slice.
func TestBuildForkLines_FirstWritableAPIErrorNotRechained(t *testing.T) {
	src := `{"type":"system","subtype":"api_error","uuid":"err0","parentUuid":"uuid-outside-slice","sessionId":"src","level":"error","retryAttempt":1,"error":{"message":"Connection error."}}
{"type":"user","uuid":"u1","parentUuid":"err0","sessionId":"src","message":{"role":"user","content":"first"}}
`
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)
	if p, isStr := rows["err0"]["parentUuid"].(string); isStr {
		t.Errorf("first-writable api_error parent = %q, want null (no predecessor exists to re-chain to)", p)
	}
	if got, want := forkedParent(t, rows, "u1"), forkedUUID(t, rows, "err0"); got != want {
		t.Errorf("u1 parent = %q, want err0 %q", got, want)
	}
}

// TestBuildForkLines_AbandonedBranchContentRowsKeepOriginalParents:
// user/assistant rows are NEVER re-chained, even when their parent
// chain departs from file order — claude's own branch walk correctly
// ignores abandoned content branches, and resurrecting one would
// corrupt resumed context.
func TestBuildForkLines_AbandonedBranchContentRowsKeepOriginalParents(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply one"}]}}
{"type":"assistant","uuid":"a1-abandoned","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"abandoned alternative"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"second"}}
`
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	rows := parseForkedByOldUUID(t, lines)
	if got, want := forkedParent(t, rows, "a1-abandoned"), forkedUUID(t, rows, "u1"); got != want {
		t.Errorf("abandoned assistant parent = %q, want original u1 %q (content rows must not be re-chained)", got, want)
	}
	if got, want := forkedParent(t, rows, "u2"), forkedUUID(t, rows, "a1"); got != want {
		t.Errorf("u2 parent = %q, want a1 %q", got, want)
	}
}

// TestBuildForkLines_RechainIdempotentOnForkOfFork: feeding a
// re-chained fork back through the transform keeps the topology stable
// — every api_error row's parent is its predecessor in both
// generations, so old broken forks heal once and stay healed.
func TestBuildForkLines_RechainIdempotentOnForkOfFork(t *testing.T) {
	src := readFixture(t, apiErrorOffBranchFixture)
	gen1ID, gen1Lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "err2", "")
	if err != nil {
		t.Fatalf("gen1: %v", err)
	}
	gen1 := parseForkedByOldUUID(t, gen1Lines)

	gen2Input := strings.Join(gen1Lines, "\n") + "\n"
	_, gen2Lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(gen2Input), gen1ID, "", "")
	if err != nil {
		t.Fatalf("gen2: %v", err)
	}
	// gen2 rows' forkedFrom points at gen1 uuids; re-index through gen1's
	// fixture names.
	gen2ByGen1UUID := parseForkedByOldUUID(t, gen2Lines)
	gen2 := make(map[string]map[string]any, len(gen1))
	for fixtureName, gen1Row := range gen1 {
		gen1UUID, _ := gen1Row["uuid"].(string)
		if row, ok := gen2ByGen1UUID[gen1UUID]; ok {
			gen2[fixtureName] = row
		}
	}

	for _, generation := range []map[string]map[string]any{gen1, gen2} {
		if got, want := forkedParent(t, generation, "err1"), forkedUUID(t, generation, "a3-final"); got != want {
			t.Errorf("err1 parent = %q, want a3-final %q (topology drifted across fork generations)", got, want)
		}
		if got, want := forkedParent(t, generation, "err2"), forkedUUID(t, generation, "err1"); got != want {
			t.Errorf("err2 parent = %q, want err1 %q", got, want)
		}
	}
}
