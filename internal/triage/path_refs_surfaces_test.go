package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// seedPathlinksWorkspace creates a temporary workspace with the given
// relative files (each path is touched as an empty file). Returns the
// workspace root so the caller can plumb it onto a test thread.
//
// Each path body must contain at least one '/' to satisfy
// `pathlinks.pathPattern`, which requires `[\w.\-~]+(?:/[\w.\-~]+)+`.
// A single-segment name like `README.md` looks valid on disk but the
// regex silently rejects it — use `docs/README.md` instead.
func seedPathlinksWorkspace(t *testing.T, relPaths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range relPaths {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("seed dir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatalf("seed file %s: %v", rel, err)
		}
	}
	return root
}

// pathRefsFromMeta is a small test helper that decodes the pathRefs
// allowlist out of an item's meta. Returns nil when the key is absent.
func pathRefsFromMeta(t *testing.T, meta string) []pathlinks.PathRef {
	t.Helper()
	if meta == "" {
		return nil
	}
	var decoded struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	return decoded.PathRefs
}

// TestProposedPlanEnrichesPathRefs covers the write-time path-link
// validation for proposed_plan rows. The plan body lives in
// evt.Content (the payload data) — distinct from the assistant_text
// case where it lives in item.Summary — so this guards the
// enrichPathRefsFromTexts wiring in handleProposedPlan.
func TestProposedPlanEnrichesPathRefs(t *testing.T) {
	wsRoot := seedPathlinksWorkspace(t, "src/foo.ts", "docs/plan.md")
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)

	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-plan",
		ProjectID:     triageTestProjectID,
		Title:         "plan path-refs",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Open the turn so handleProposedPlan can resolve currentTurnIndex.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-plan",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Plan body contains a real path, a bogus path-shaped token, and
	// inline docs reference. Validator should keep `src/foo.ts` and
	// `docs/plan.md`, drop `bogus.nope/file.bad`.
	planBody := "## Plan\n\n- Touch src/foo.ts and update docs/plan.md\n- Delete bogus.nope/file.bad\n"
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t-plan",
		ItemID:    "plan-1",
		Content:   planBody,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("proposed plan: %v", err)
	}

	item, found, err := st.GetThreadItem("t-plan", "plan-1")
	if err != nil || !found {
		t.Fatalf("get plan item: found=%v err=%v", found, err)
	}
	refs := pathRefsFromMeta(t, item.Meta)
	if len(refs) == 0 {
		t.Fatalf("expected pathRefs on proposed_plan meta, got meta=%q", item.Meta)
	}
	gotPaths := make(map[string]int)
	for _, ref := range refs {
		gotPaths[ref.Path]++
	}
	if gotPaths["src/foo.ts"] == 0 {
		t.Errorf("expected src/foo.ts in pathRefs, got %v", refs)
	}
	if gotPaths["docs/plan.md"] == 0 {
		t.Errorf("expected docs/plan.md in pathRefs, got %v", refs)
	}
	if gotPaths["bogus.nope/file.bad"] != 0 {
		t.Errorf("did not expect bogus.nope/file.bad in pathRefs, got %v", refs)
	}
}

// TestAskUserQuestionEnrichesPathRefs covers the AskUserQuestion case:
// the question/option text lives inside meta.input.questions[] (the
// SAME shape Codex's request_user_input uses). userInputValidationTexts
// must walk both Claude and Codex shapes; this test exercises the
// Claude side because that's the one that reaches persistToolCallLaunch
// via a real wire event flow.
func TestAskUserQuestionEnrichesPathRefs(t *testing.T) {
	wsRoot := seedPathlinksWorkspace(t, "src/login.ts", "src/signup.ts", "docs/README.md")
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)

	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-ask",
		ProjectID:     triageTestProjectID,
		Title:         "ask path-refs",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-ask",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Build a real AskUserQuestion meta. Validator scope must match
	// the renderer scope (AskUserQuestionCard.svelte): question text
	// and option.preview flow through ChatMarkdown so they MUST be
	// validated; option.label and option.description render as plain
	// `<p>` text so they MUST NOT be validated — otherwise the
	// allowlist would contain paths the user can never click.
	meta, err := json.Marshal(map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []map[string]any{
				{
					"id":       "q1",
					"header":   "Auth method",
					"question": "Which file should host auth — src/login.ts or src/signup.ts?",
					"options": []map[string]any{
						{
							// label/description render as plain <p>; paths here
							// MUST NOT validate.
							"label":       "Use docs/README.md",
							"description": "Lives next to bogus.nope/file.bad",
							// preview renders through ChatMarkdown; paths here
							// MUST validate.
							"preview": "Suggested change in src/login.ts",
						},
						{
							"label":       "Use bogus.nope/file.bad",
							"description": "Not a real path",
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t-ask",
		ItemID:    "ask-1",
		ItemType:  "AskUserQuestion",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	item, found, err := st.GetThreadItem("t-ask", "ask-1")
	if err != nil || !found {
		t.Fatalf("get ask item: found=%v err=%v", found, err)
	}
	refs := pathRefsFromMeta(t, item.Meta)
	if len(refs) == 0 {
		t.Fatalf("expected pathRefs on AskUserQuestion meta, got meta=%q", item.Meta)
	}
	got := make(map[string]int)
	for _, ref := range refs {
		got[ref.Path]++
	}
	if got["src/login.ts"] == 0 {
		t.Errorf("expected src/login.ts (from question + preview) in pathRefs, got %v", refs)
	}
	if got["src/signup.ts"] == 0 {
		t.Errorf("expected src/signup.ts (from question) in pathRefs, got %v", refs)
	}
	if got["docs/README.md"] != 0 {
		t.Errorf("did not expect docs/README.md (option.label) in pathRefs — labels render as plain text, got %v", refs)
	}
	if got["bogus.nope/file.bad"] != 0 {
		t.Errorf("did not expect bogus.nope/file.bad in pathRefs, got %v", refs)
	}
}

// TestAskUserQuestionRequestUserInputAlsoValidates pins
// userInputValidationTexts' Codex branch via the full router path. A
// bug in the router wiring for `request_user_input` (e.g. the tool
// name slipping out of the switch in persistToolCallLaunch) would
// not be caught by the helper-only assertion below — the second case
// here drives the same item through Router.Handle and reads the
// persisted meta back out.
func TestAskUserQuestionRequestUserInputAlsoValidates(t *testing.T) {
	wsRoot := seedPathlinksWorkspace(t, "config/app.yaml")

	metaJSON, err := json.Marshal(map[string]any{
		"toolName": "request_user_input",
		"input": map[string]any{
			"questions": []map[string]any{
				{
					"question": "Edit config/app.yaml?",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}

	t.Run("helper-level scope", func(t *testing.T) {
		texts := userInputValidationTexts("request_user_input", string(metaJSON))
		if len(texts) == 0 {
			t.Fatalf("expected validation texts for request_user_input, got none")
		}
		refs := pathlinks.ExtractAndValidate(wsRoot, texts[0])
		if len(refs) != 1 || refs[0].Path != "config/app.yaml" {
			t.Fatalf("expected single ref config/app.yaml, got %#v", refs)
		}
	})

	t.Run("full router path persists pathRefs onto the codex item meta", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		ensureTriageProject(t, st)
		now := time.Now().UnixMilli()
		if err := st.CreateThread(store.Thread{
			ID:            "t-codex-input",
			ProjectID:     triageTestProjectID,
			Title:         "request_user_input path-refs",
			Provider:      "codex",
			WorkspacePath: wsRoot,
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			t.Fatalf("create thread: %v", err)
		}
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  "t-codex-input",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn start: %v", err)
		}
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  "t-codex-input",
			ItemID:    "rui-1",
			ItemType:  "request_user_input",
			Meta:      metaJSON,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start: %v", err)
		}
		item, found, err := st.GetThreadItem("t-codex-input", "rui-1")
		if err != nil || !found {
			t.Fatalf("get codex item: found=%v err=%v", found, err)
		}
		refs := pathRefsFromMeta(t, item.Meta)
		if len(refs) != 1 || refs[0].Path != "config/app.yaml" {
			t.Fatalf("expected persisted pathRefs=[config/app.yaml], got %#v (meta=%q)", refs, item.Meta)
		}
	})
}

// TestUserInputValidationTextsIgnoresUnrelatedTools is a quick negative
// test — only AskUserQuestion / request_user_input should produce text
// sources. Any other tool (Bash, Edit, …) returns no texts so the
// validator doesn't run on unrelated meta.
func TestUserInputValidationTextsIgnoresUnrelatedTools(t *testing.T) {
	meta := `{"toolName":"Bash","input":{"command":"ls src/foo.ts"}}`
	if got := userInputValidationTexts("Bash", meta); got != nil {
		t.Fatalf("expected no texts for Bash, got %v", got)
	}
	if got := userInputValidationTexts("Edit", meta); got != nil {
		t.Fatalf("expected no texts for Edit, got %v", got)
	}
}

// TestAdvisorCompletionEnrichesPathRefs exercises the advisor-result
// branch in persistToolCallCompletion. The advisor body lives in
// evt.Content (the payload data, not item.Summary which is just the
// literal "advisor"), so this guards the explicit text-source plumbing.
func TestAdvisorCompletionEnrichesPathRefs(t *testing.T) {
	wsRoot := seedPathlinksWorkspace(t, "src/router.ts")
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)

	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-advisor",
		ProjectID:     triageTestProjectID,
		Title:         "advisor path-refs",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-advisor",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Open the advisor row first so a launch exists at completion time.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "advisor",
		"advisor_model": "claude-opus-4-7",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t-advisor",
		ItemID:    "adv-1",
		ItemType:  "advisor",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{
		"is_error": false,
	})
	advisorBody := "Reviewed src/router.ts and bogus.nope/file.bad — the router has an issue."
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t-advisor",
		ItemID:    "adv-1",
		Content:   advisorBody,
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	item, found, err := st.GetThreadItem("t-advisor", "adv-1")
	if err != nil || !found {
		t.Fatalf("get advisor item: found=%v err=%v", found, err)
	}
	refs := pathRefsFromMeta(t, item.Meta)
	if len(refs) == 0 {
		t.Fatalf("expected pathRefs on advisor item meta, got meta=%q", item.Meta)
	}
	got := make(map[string]int)
	for _, ref := range refs {
		got[ref.Path]++
	}
	if got["src/router.ts"] == 0 {
		t.Errorf("expected src/router.ts in pathRefs, got %v", refs)
	}
	if got["bogus.nope/file.bad"] != 0 {
		t.Errorf("did not expect bogus.nope/file.bad in pathRefs, got %v", refs)
	}
}
