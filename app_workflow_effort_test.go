package main

import (
	"testing"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
)

// effortTestModel is a Claude model whose advertised tiers are a strict subset
// of the vocabulary: low → xhigh plus max, defaulting to xhigh. It is what makes
// "authored `ultra` is coerced" a real assertion rather than a tautology.
const effortTestModel = "claude-opus-4-7"

// TestWorkflowEffortTiersMatchTheProviderReasoningEfforts is the drift guard for
// the one vocabulary that is deliberately declared twice.
//
// `internal/workflow/def` is a pure package — it validates and publishes a
// workflow definition with no provider in reach — so it cannot import
// `internal/provider` to reuse its tier constants. The cost of that purity is
// two lists, and this is what keeps them one vocabulary: a tier added to the
// provider enum but not to def would validate as "unknown effort", and a tier
// added to def but not to the provider would be accepted by validation and then
// coerced away at thread creation, both silently.
func TestWorkflowEffortTiersMatchTheProviderReasoningEfforts(t *testing.T) {
	authored := def.EffortTiers()
	runtime := provider.AllReasoningEfforts

	inDef := make(map[string]bool, len(authored))
	for _, tier := range authored {
		inDef[string(tier)] = true
	}
	inProvider := make(map[string]bool, len(runtime))
	for _, effort := range runtime {
		inProvider[string(effort)] = true
	}
	for name := range inDef {
		if !inProvider[name] {
			t.Errorf("def declares effort tier %q but provider.AllReasoningEfforts does not; a workflow could author a tier no session can run", name)
		}
	}
	for name := range inProvider {
		if !inDef[name] {
			t.Errorf("provider.AllReasoningEfforts declares %q but def does not; workflow validation would refuse a tier the app supports", name)
		}
	}

	// Order is part of the contract too: it is what the published schema's enum
	// and every diagnostic list, so a reordered provider enum should surface here
	// rather than as an inconsistently ordered picker.
	if len(authored) == len(runtime) {
		for index := range authored {
			if string(authored[index]) != string(runtime[index]) {
				t.Errorf("tier %d: def has %q, provider has %q; the two lists must stay in the same order", index, authored[index], runtime[index])
			}
		}
	}
}

// TestWorkflowThreadIgnoresRememberedChatProfile is the no-bleed proof. A
// workflow lane is a deterministic piece of a definition, so its model settings
// must come from the catalog's defaults for (provider, model) — never from
// `chat_model_profiles`, which records how the user happened to configure their
// last interactive chat on the same model.
func TestWorkflowThreadIgnoresRememberedChatProfile(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	catalogDefault := chatmodel.FallbackProfile(string(provider.Claude), effortTestModel)
	remembered := store.ChatModelProfile{
		Provider:        string(provider.Claude),
		Model:           effortTestModel,
		ReasoningEffort: string(provider.EffortLow),
		FastMode:        true,
		ContextWindow:   catalogDefault.ContextWindow,
		RuntimeMode:     string(provider.DefaultRuntimeMode),
	}
	if remembered.ReasoningEffort == catalogDefault.ReasoningEffort {
		t.Fatalf("fixture is inert: remembered effort %q equals the catalog default", remembered.ReasoningEffort)
	}
	if err := app.store.UpsertChatModelProfile(remembered); err != nil {
		t.Fatal(err)
	}
	// Guard the guard: the chat path really would pick the remembered profile up.
	if seeded := app.seedChatModelProfile(string(provider.Claude), effortTestModel); seeded.ReasoningEffort != remembered.ReasoningEffort {
		t.Fatalf("chat seed effort = %q, want the remembered %q — fixture no longer proves anything", seeded.ReasoningEffort, remembered.ReasoningEffort)
	}

	thread := createWorkflowThreadForTest(t, app, projectRow, repo, "")
	if thread.ReasoningEffort != catalogDefault.ReasoningEffort {
		t.Errorf("workflow thread effort = %q, want the catalog default %q (the remembered chat profile bled through)",
			thread.ReasoningEffort, catalogDefault.ReasoningEffort)
	}
	if thread.FastMode {
		t.Error("workflow thread inherited fast mode from the remembered chat profile")
	}
}

// TestWorkflowThreadTakesAuthoredEffort covers the two ends of the authored
// field: a tier the model advertises lands verbatim, and one it does not is
// coerced onto the model's own default rather than stored illegally —
// `threads.reasoning_effort` is CHECKed, so an uncoerced value is a write error,
// not a cosmetic one.
func TestWorkflowThreadTakesAuthoredEffort(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	catalogDefault := chatmodel.FallbackProfile(string(provider.Claude), effortTestModel).ReasoningEffort
	cases := []struct {
		name     string
		authored string
		want     string
	}{
		{"supported tier is honoured", string(provider.EffortMedium), string(provider.EffortMedium)},
		// `ultra` is a Codex-only tier; Claude models advertise none of it.
		{"unsupported tier is coerced", string(provider.EffortUltra), catalogDefault},
		{"unset falls back to the catalog default", "", catalogDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.authored != "" && !def.KnownEffortTier(tc.authored) {
				t.Fatalf("fixture authors %q, which workflow validation would refuse", tc.authored)
			}
			thread := createWorkflowThreadForTest(t, app, projectRow, repo, tc.authored)
			if thread.ReasoningEffort != tc.want {
				t.Fatalf("thread effort = %q, want %q", thread.ReasoningEffort, tc.want)
			}
			// Whatever landed has to be a tier the model can actually be started
			// with, which is the property the CHECK constraint encodes.
			if !app.reasoningEffortSupportedForModel(string(provider.Claude), thread.Model, thread.ReasoningEffort) {
				t.Fatalf("thread effort %q is not supported by %s", thread.ReasoningEffort, thread.Model)
			}
		})
	}
}

// createWorkflowThreadForTest drives the real creation path with one authored
// effort, reading the row back the way every later session start does.
func createWorkflowThreadForTest(t *testing.T, app *App, projectRow store.Project, workspace, effort string) store.Thread {
	t.Helper()
	thread, err := app.createWorkflowThread(workflowThreadSpec{
		itemID: "item-effort", label: `phase "survey"`,
		title:        workflowThreadTitle("Survey", "survey"),
		providerName: string(provider.Claude), model: effortTestModel,
		effort:    effort,
		access:    def.AccessReadOnly,
		workspace: preparedWorkflowWorkspace{path: workspace, project: projectRow},
	})
	if err != nil {
		t.Fatal(err)
	}
	return thread
}
