package triage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider/claude"
)

// Both fixtures must persist the real two-hunk inline diff through Claude's
// parser and the Router. A plain-string tool result would leave a disabled
// diff header instead of exercising body rendering and lazy payload loading.
func TestFileEditDiffScenarioPersistsAnInlineDiffPayload(t *testing.T) {
	for _, name := range []string{"file-edit-diff", "bench-mixed-turn"} {
		t.Run(name, func(t *testing.T) { testScenarioInlineDiff(t, name) })
	}
}

func testScenarioInlineDiff(t *testing.T, name string) {
	const threadID = "t1"
	// createTestThread pins the thread workspace at /tmp, and the mock
	// runs with the workspace as its cwd, so ${CWD} must bind to the
	// same root for the display path to normalize to a relative one.
	vars := scenario.Vars{
		"SESSION_ID": "sess-test",
		"TURN":       "1",
		"CWD":        "/tmp",
	}

	_, s, err := scenario.LoadLibrary(name)
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}

	router, st, _ := newTestRouter(t)
	createTestThread(t, st, threadID)

	parser := claude.NewParser()
	// The mock's claude adapter owns the per-turn system/init frame (a
	// scenario may not emit one), and triage opens the logical turn on
	// it — so the test supplies the adapter's frame, not the scenario's.
	lines := []string{`{"type":"system","subtype":"init","session_id":"sess-test","model":"claude-opus-4-7","cwd":"/tmp","tools":["Edit"],"claude_code_version":"2.99.0"}`}
	for _, turn := range s.Turns {
		for _, step := range turn.Steps {
			if step.Emit == nil {
				continue
			}
			for _, line := range step.Emit.Lines {
				lines = append(lines, vars.Substitute(line))
			}
		}
	}

	for _, line := range lines {
		events, err := parser.ParseLine(threadID, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		for _, evt := range events {
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle %s: %v", evt.Kind, err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := router.Wait(ctx); err != nil {
		t.Fatalf("drain router: %v", err)
	}

	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}

	var (
		meta  ToolResultMeta
		patch string
		found bool
	)
	for _, item := range items {
		if item.PayloadID == "" {
			continue
		}
		pm, err := st.GetPayloadMeta(threadID, item.PayloadID)
		if err != nil || pm.Kind != toolResultPayloadKind {
			continue
		}
		var candidate ToolResultMeta
		if json.Unmarshal([]byte(pm.Meta), &candidate) != nil || candidate.ItemType != "file_change" {
			continue
		}
		data, err := st.GetPayloadData(threadID, item.PayloadID)
		if err != nil {
			t.Fatalf("read payload data: %v", err)
		}
		meta, patch, found = candidate, string(data), true
		break
	}
	if !found {
		t.Fatal("no file_change tool_result payload persisted; the scenario's tool_result carried no structured patch")
	}

	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", meta.InlineDiff)
	}
	if len(meta.InlineDiff.Files) != 1 || meta.InlineDiff.Files[0].Path != "src/settings.ts" {
		t.Fatalf("files = %+v, want one entry for src/settings.ts", meta.InlineDiff.Files)
	}
	file := meta.InlineDiff.Files[0]
	// The body is the point: a preview with no lines renders as the same
	// disabled header the plain-string scenario produces.
	if file.PreviewLineCount == 0 || file.PreviewTruncated {
		t.Fatalf("preview = %d lines (truncated %t), want a complete non-empty body", file.PreviewLineCount, file.PreviewTruncated)
	}
	if file.Insertions != 6 || file.Deletions != 4 {
		t.Fatalf("counts = +%d -%d, want +6 -4", file.Insertions, file.Deletions)
	}
	if strings.Count(file.PreviewPatch, "\n@@ ") != 2 || !strings.HasPrefix(file.PreviewPatch, "diff --git ") {
		t.Fatalf("preview patch is not a two-hunk unified diff:\n%s", file.PreviewPatch)
	}
	if hunks := strings.Count(patch, "\n@@ "); hunks != 2 {
		t.Fatalf("persisted patch has %d hunks, want 2:\n%s", hunks, patch)
	}
	for _, want := range []string{
		"diff --git a/src/settings.ts b/src/settings.ts",
		"@@ -1,7 +1,8 @@",
		"@@ -12,5 +13,6 @@",
		"-  retries: 3,",
		"+  retries: 5,",
		"+  return { ...config, ...pick(input) };",
	} {
		if !strings.Contains(patch, want) {
			t.Fatalf("persisted patch is missing %q:\n%s", want, patch)
		}
	}
}
