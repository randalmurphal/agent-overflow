package sessionimport

import (
	"path/filepath"
	"strings"
	"testing"
)

func chainUUIDs(branch Branch) []string {
	out := make([]string, 0, len(branch.Chain))
	for _, row := range branch.Chain {
		out = append(out, row.UUID)
	}
	return out
}

func TestBuildBranchesLinearChain(t *testing.T) {
	branch := buildChain(t,
		userRow("u1", "", "hello", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("hi")}, "2026-01-01T00:00:01.000Z"),
	)
	if got, want := strings.Join(chainUUIDs(branch), ","), "u1,a1"; got != want {
		t.Errorf("chain = %s, want %s", got, want)
	}
	if branch.LeafUUID != "a1" {
		t.Errorf("leaf = %q, want a1", branch.LeafUUID)
	}
	if want := parseISOMillis("2026-01-01T00:00:01.000Z"); branch.LastActivityAt != want {
		t.Errorf("lastActivityAt = %d, want %d", branch.LastActivityAt, want)
	}
}

func TestBuildBranchesEnumeratesEveryLeaf(t *testing.T) {
	rows := decodeBranchRows(t,
		userRow("u1", "", "shared prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("first answer")}, "2026-01-01T00:00:01.000Z"),
		// A revert-and-retry re-parents a second prompt onto u1's answer,
		// leaving the first continuation as an abandoned sibling branch.
		userRow("u2", "a1", "branch one", "2026-01-01T00:00:02.000Z"),
		assistantRow("a2", "u2", "msg_2", []any{textBlock("one")}, "2026-01-01T00:00:03.000Z"),
		userRow("u3", "a1", "branch two", "2026-01-01T00:00:04.000Z"),
		assistantRow("a3", "u3", "msg_3", []any{textBlock("two")}, "2026-01-01T00:00:05.000Z"),
	)
	leafTitles := map[string]string{"a2": "branch one", "a3": "branch two"}

	branches, warnings := BuildBranches(rows, leafTitles)
	if len(branches) != 2 {
		t.Fatalf("got %d branches, want 2: %+v", len(branches), branches)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
	if got, want := strings.Join(chainUUIDs(branches[0]), ","), "u1,a1,u2,a2"; got != want {
		t.Errorf("branch 0 chain = %s, want %s", got, want)
	}
	if got, want := strings.Join(chainUUIDs(branches[1]), ","), "u1,a1,u3,a3"; got != want {
		t.Errorf("branch 1 chain = %s, want %s", got, want)
	}
	if branches[0].Title != "branch one" || branches[1].Title != "branch two" {
		t.Errorf("titles = %q / %q", branches[0].Title, branches[1].Title)
	}
}

func TestBuildBranchesTitleFallsBackToLastUserPrompt(t *testing.T) {
	branch := buildChain(t,
		userRow("u1", "", "only prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("hi")}, "2026-01-01T00:00:01.000Z"),
	)
	if branch.Title != "only prompt" {
		t.Errorf("title = %q, want the last user prompt", branch.Title)
	}
}

func TestBuildBranchesSkipsProgressAncestors(t *testing.T) {
	branch := buildChain(t,
		userRow("u1", "", "hello", "2026-01-01T00:00:00.000Z"),
		map[string]any{"type": "progress", "uuid": "p1", "parentUuid": "u1", "isSidechain": false},
		assistantRow("a1", "p1", "msg_1", []any{textBlock("hi")}, "2026-01-01T00:00:01.000Z"),
	)
	if got, want := strings.Join(chainUUIDs(branch), ","), "u1,a1"; got != want {
		t.Errorf("chain = %s, want %s (progress must be transparent, not a branch)", got, want)
	}
}

func TestBuildBranchesStitchesCompactionThroughLogicalParent(t *testing.T) {
	branch := buildChain(t,
		userRow("u1", "", "before compaction", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("long answer")}, "2026-01-01T00:00:01.000Z"),
		map[string]any{
			"type": "system", "subtype": "compact_boundary",
			"uuid": "b1", "parentUuid": nil, "logicalParentUuid": "a1",
			"isSidechain": false, "content": "Conversation compacted",
			"timestamp":       "2026-01-01T00:00:02.000Z",
			"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 340000},
		},
		userRow("s1", "b1", "This session is being continued…", "2026-01-01T00:00:03.000Z",
			with("isCompactSummary", true), with("isVisibleInTranscriptOnly", true)),
		userRow("u2", "s1", "carry on", "2026-01-01T00:00:04.000Z"),
	)
	if got, want := strings.Join(chainUUIDs(branch), ","), "u1,a1,b1,s1,u2"; got != want {
		t.Errorf("chain = %s, want %s (compaction must stitch through logicalParentUuid)", got, want)
	}
}

func TestBuildBranchesReattachesOrphanedParallelToolResult(t *testing.T) {
	// Two parallel tool_uses on one assistant message. The first result
	// row was never written (crash mid-tool), so the second result's
	// parentUuid dangles — sourceToolAssistantUUID is what recovers it.
	rows := decodeBranchRows(t,
		userRow("u1", "", "run both", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_1", "Bash", map[string]any{"command": "ls"}),
			toolUseBlock("toolu_2", "Bash", map[string]any{"command": "pwd"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r2", "r1-never-written", "toolu_2", "/repo", "2026-01-01T00:00:02.000Z",
			with("sourceToolAssistantUUID", "a1")),
	)
	branches, warnings := BuildBranches(rows, nil)
	if len(branches) != 1 {
		t.Fatalf("got %d branches, want 1: %+v", len(branches), branches)
	}
	if got, want := strings.Join(chainUUIDs(branches[0]), ","), "u1,a1,r2"; got != want {
		t.Errorf("chain = %s, want %s", got, want)
	}
	if !hasWarning(warnings, WarnOrphanedToolResult) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnOrphanedToolResult)
	}
}

func TestBuildBranchesSkipsInlineSidechainRows(t *testing.T) {
	rows := decodeBranchRows(t,
		userRow("u1", "", "spawn an agent", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("ok")}, "2026-01-01T00:00:01.000Z"),
		userRow("sc1", "", "subagent prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
		assistantRow("sc2", "sc1", "msg_2", []any{textBlock("subagent answer")}, "2026-01-01T00:00:03.000Z",
			with("isSidechain", true)),
	)
	branches, warnings := BuildBranches(rows, nil)
	if len(branches) != 1 {
		t.Fatalf("got %d branches, want 1 (inline sidechains must not enumerate): %+v", len(branches), branches)
	}
	if !hasWarning(warnings, WarnInlineSidechain) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnInlineSidechain)
	}
}

func TestBuildBranchesDropsDuplicateUUIDs(t *testing.T) {
	rows := decodeBranchRows(t,
		userRow("u1", "", "hello", "2026-01-01T00:00:00.000Z"),
		userRow("u1", "", "hello again", "2026-01-01T00:00:01.000Z"),
	)
	branches, warnings := BuildBranches(rows, nil)
	if len(branches) != 1 {
		t.Fatalf("got %d branches, want 1", len(branches))
	}
	if !hasWarning(warnings, WarnDuplicateUUID) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnDuplicateUUID)
	}
}

func TestBuildBranchesEmptyTranscript(t *testing.T) {
	branches, warnings := BuildBranches(nil, nil)
	if len(branches) != 0 {
		t.Fatalf("got %d branches, want 0", len(branches))
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

func TestBuildBranchesSurvivesParentCycle(t *testing.T) {
	rows := decodeBranchRows(t,
		userRow("c1", "c2", "cycle", "2026-01-01T00:00:00.000Z"),
		userRow("c2", "c1", "cycle", "2026-01-01T00:00:01.000Z"),
	)
	branches, warnings := BuildBranches(rows, nil)
	if len(branches) != 0 {
		t.Fatalf("a cycle must not produce a branch, got %+v", branches)
	}
	if !hasWarning(warnings, WarnNoLeaf) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnNoLeaf)
	}
}

func TestLoadSessionReadsTitlesAndToleratesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		"}}{ not json",
		userRow("u1", "", "start", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("one")}, "2026-01-01T00:00:01.000Z"),
		userRow("u2", "a1", "second branch", "2026-01-01T00:00:02.000Z"),
		assistantRow("a2", "u2", "msg_2", []any{textBlock("two")}, "2026-01-01T00:00:03.000Z"),
		userRow("u3", "a1", "third branch", "2026-01-01T00:00:04.000Z"),
		assistantRow("a3", "u3", "msg_3", []any{textBlock("three")}, "2026-01-01T00:00:05.000Z"),
		map[string]any{"type": "last-prompt", "lastPrompt": "second branch", "leafUuid": "a2", "sessionId": sessionA},
		map[string]any{"type": "last-prompt", "lastPrompt": "third branch", "leafUuid": "a3", "sessionId": sessionA},
		`{"type":"assistant","uuid":"trunc","message":{"content":[{"type":"text","text":"cut off`,
	)

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	defer loaded.Close()
	if loaded.SessionID != sessionA {
		t.Errorf("sessionID = %q, want %q", loaded.SessionID, sessionA)
	}
	if want := filepath.Join(dir, sessionA); loaded.SessionDir != want {
		t.Errorf("sessionDir = %q, want %q", loaded.SessionDir, want)
	}
	if len(loaded.Branches) != 2 {
		t.Fatalf("got %d branches, want 2", len(loaded.Branches))
	}
	if loaded.Branches[0].Title != "second branch" || loaded.Branches[1].Title != "third branch" {
		t.Errorf("branch titles = %q / %q", loaded.Branches[0].Title, loaded.Branches[1].Title)
	}
	for i := range loaded.Branches {
		// Pass 1 keeps skeletons only: no line body survives the scan, and
		// the events exist only for the branch actually being converted.
		for _, row := range loaded.Branches[i].Chain {
			if row.Raw != nil {
				t.Fatalf("branch %d row %s kept a decoded body after the scan", i, row.UUID)
			}
		}
		branch, err := loaded.ConvertBranch(i)
		if err != nil {
			t.Fatalf("ConvertBranch(%d): %v", i, err)
		}
		if len(branch.Events) == 0 {
			t.Errorf("branch %d converted to zero events", i)
		}
	}
}

func TestConvertBranchFillsTheTitleFallbackFromTheDecodedChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "only prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("hi")}, "2026-01-01T00:00:01.000Z"),
	)
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	defer loaded.Close()
	if loaded.Branches[0].Title != "" {
		t.Errorf("skeleton branch title = %q, want empty (the text is not in the skeleton)", loaded.Branches[0].Title)
	}
	branch, err := loaded.ConvertBranch(0)
	if err != nil {
		t.Fatalf("ConvertBranch: %v", err)
	}
	if branch.Title != "only prompt" {
		t.Errorf("converted branch title = %q, want the last user prompt", branch.Title)
	}
}

func TestFindReusablePrefixCutsOnlyAtACompleteTurnBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "shared prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("shared answer")}, "2026-01-01T00:00:01.000Z"),
		userRow("u2a", "a1", "branch A", "2026-01-01T00:00:02.000Z"),
		assistantRow("a2a", "u2a", "msg_2a", []any{textBlock("answer A")}, "2026-01-01T00:00:03.000Z"),
		userRow("u2b", "a1", "branch B", "2026-01-01T00:00:04.000Z"),
		assistantRow("a2b", "u2b", "msg_2b", []any{textBlock("answer B")}, "2026-01-01T00:00:05.000Z"),
	)
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	defer loaded.Close()
	if len(loaded.Branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(loaded.Branches))
	}
	if err := loaded.AddReusablePrefixDonor(0); err != nil {
		t.Fatalf("AddReusablePrefixDonor: %v", err)
	}
	prefix, ok, err := loaded.FindReusablePrefix(1)
	if err != nil {
		t.Fatalf("FindReusablePrefix: %v", err)
	}
	if !ok || prefix.DonorIndex != 0 || prefix.SuffixRow != 2 || prefix.NextTurnIndex != 2 {
		t.Fatalf("prefix = %+v ok=%v, want donor 0 row 2 turn 2", prefix, ok)
	}
	suffix, err := loaded.ConvertBranchFrom(1, prefix.SuffixRow)
	if err != nil {
		t.Fatalf("ConvertBranchFrom: %v", err)
	}
	for _, event := range suffix.Events {
		if event.SourceUUID == "u1" || event.SourceUUID == "a1" {
			t.Fatalf("shared-prefix event %s was converted again", event.SourceUUID)
		}
	}
	foundPrompt, foundAnswer := false, false
	for _, event := range suffix.Events {
		foundPrompt = foundPrompt || event.SourceUUID == "u2b"
		foundAnswer = foundAnswer || event.SourceUUID == "a2b"
	}
	if !foundPrompt || !foundAnswer {
		t.Fatalf("suffix events omitted branch content: %+v", suffix.Events)
	}
}

func TestFindReusablePrefixRejectsAMidFirstTurnFork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("answer A")}, "2026-01-01T00:00:01.000Z"),
		assistantRow("a2", "u1", "msg_2", []any{textBlock("answer B")}, "2026-01-01T00:00:02.000Z"),
	)
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	defer loaded.Close()
	if err := loaded.AddReusablePrefixDonor(0); err != nil {
		t.Fatalf("AddReusablePrefixDonor: %v", err)
	}
	if _, ok, err := loaded.FindReusablePrefix(1); err != nil || ok {
		t.Fatalf("mid-turn prefix = ok:%v err:%v, want no reuse", ok, err)
	}
}

func TestLoadSessionMissingFile(t *testing.T) {
	if _, err := LoadSession(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("LoadSession over a missing file: want error, got nil")
	}
}
