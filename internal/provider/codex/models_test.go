package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestListModelsUsesCodexMetadataAndStaticContextWindows(t *testing.T) {
	binary := writeModelListFakeCodex(t, `[{"model":"gpt-5.5","displayName":"gpt-5.5","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Fast responses with lighter reasoning"},{"reasoningEffort":"high","description":"Greater reasoning depth for complex problems"},{"reasoningEffort":"xhigh","description":"Extra high reasoning depth for complex problems"}],"defaultReasoningEffort":"high","serviceTiers":[{"id":"priority","name":"Fast","description":"1.5x speed, increased usage"}]},{"model":"legacy-hidden","displayName":"Legacy Hidden","hidden":true,"supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Low"}],"defaultReasoningEffort":"low","serviceTiers":[]}]`)

	models, err := ListModels(context.Background(), ModelListConfig{
		Binary:       binary,
		CustomModels: []string{"custom-codex"},
	})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2: %#v", len(models), models)
	}

	model := models[0]
	if model.Slug != "gpt-5.5" {
		t.Fatalf("Slug = %q, want gpt-5.5", model.Slug)
	}
	if model.Name != "GPT 5.5" {
		t.Errorf("Name = %q, want GPT 5.5", model.Name)
	}
	if !contains(model.Capabilities, provider.ModelCapabilityFastMode) {
		t.Errorf("Capabilities = %#v, want fast mode", model.Capabilities)
	}
	// The whole wire tier rides along, not just the id: the name is what the
	// composer labels the toggle and the description is its tooltip.
	if model.FastModeTier == nil {
		t.Fatalf("FastModeTier = nil, want the wire priority tier")
	}
	if *model.FastModeTier != (provider.FastModeTier{
		ID:          "priority",
		Name:        "Fast",
		Description: "1.5x speed, increased usage",
	}) {
		t.Errorf("FastModeTier = %#v, want the full wire tier", *model.FastModeTier)
	}
	if len(model.ContextWindows) != 1 ||
		model.ContextWindows[0].Tokens != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindows = %#v, want codex standard only", model.ContextWindows)
	}
	if len(model.ReasoningEfforts) != 3 {
		t.Fatalf("ReasoningEfforts len = %d, want 3", len(model.ReasoningEfforts))
	}
	if model.ReasoningEfforts[1].Slug != "high" || !model.ReasoningEfforts[1].Default {
		t.Errorf("ReasoningEfforts[1] = %#v, want default high", model.ReasoningEfforts[1])
	}
	if model.ReasoningEfforts[0].Label != "Low" ||
		model.ReasoningEfforts[1].Label != "High" ||
		model.ReasoningEfforts[2].Label != "xHigh" {
		t.Errorf("ReasoningEfforts labels = %#v, want canonical tier labels", model.ReasoningEfforts)
	}

	custom := models[1]
	if custom.Slug != "custom-codex" || !custom.IsCustom {
		t.Fatalf("custom model = %#v, want custom-codex marked custom", custom)
	}
	if !contains(custom.Capabilities, provider.ModelCapabilityFastMode) {
		t.Errorf("custom Capabilities = %#v, want copied fast mode", custom.Capabilities)
	}
	if len(custom.ReasoningEfforts) != len(model.ReasoningEfforts) {
		t.Errorf("custom reasoning efforts len = %d, want %d", len(custom.ReasoningEfforts), len(model.ReasoningEfforts))
	}
	// A custom slug inherits the template's tier — otherwise it would claim
	// fast-mode support and then have no id to send — but must not SHARE the
	// pointer, or mutating one entry's tier would rewrite the other's.
	if custom.FastModeTier == nil || *custom.FastModeTier != *model.FastModeTier {
		t.Fatalf("custom FastModeTier = %#v, want a copy of %#v", custom.FastModeTier, model.FastModeTier)
	}
	if custom.FastModeTier == model.FastModeTier {
		t.Error("custom FastModeTier aliases the template's pointer, want an independent copy")
	}
}

// TestCodexFastModeTierIsWireDriven pins the whole selection rule. `serviceTiers`
// is the model's full tier menu — upstream ships flex/batch on it — so the fast
// entry is identified by an anchor, never assumed from position or presence.
func TestCodexFastModeTierIsWireDriven(t *testing.T) {
	tests := []struct {
		name  string
		model codexModel
		want  *provider.FastModeTier
	}{
		{
			name:  "no tiers at all",
			model: codexModel{},
			want:  nil,
		},
		{
			name: "canonical priority tier carries its wire label",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "priority", Name: "Fast", Description: "1.5x speed, increased usage"},
			}},
			want: &provider.FastModeTier{ID: "priority", Name: "Fast", Description: "1.5x speed, increased usage"},
		},
		{
			name: "priority wins over an earlier non-fast tier",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "flex", Name: "Flex"},
				{ID: "priority", Name: "Fast"},
			}},
			want: &provider.FastModeTier{ID: "priority", Name: "Fast"},
		},
		{
			// The id-rename case: upstream moves the fast tier off `priority`
			// but keeps calling it "fast". AO follows the id it was given.
			name: "renamed id still matches on the wire name",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "turbo", Name: "fast", Description: "2x speed"},
			}},
			want: &provider.FastModeTier{ID: "turbo", Name: "fast", Description: "2x speed"},
		},
		{
			// The display-rename case: the label moves, the id anchor holds,
			// and the new label is what the composer shows.
			name: "renamed display name still matches on the id",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "priority", Name: "Turbo", Description: "2x speed"},
			}},
			want: &provider.FastModeTier{ID: "priority", Name: "Turbo", Description: "2x speed"},
		},
		{
			// The regression this rule exists for: flex is SLOWER. Reporting it
			// as fast-capable would put a "Flex Mode / On" toggle in the
			// composer that makes turns worse.
			name: "a flex-only model is not fast capable",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "flex", Name: "Flex", Description: "slower, discounted"},
			}},
			want: nil,
		},
		{
			name: "an unknown tier is not assumed to be fast",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "batch", Name: "slow", Description: "lower priority"},
			}},
			want: nil,
		},
		{
			name: "a tier with no id cannot be requested",
			model: codexModel{ServiceTiers: []codexModelServiceTier{
				{ID: "  ", Name: "Fast"},
			}},
			want: nil,
		},
		{
			// The deprecated key has no tier metadata, so the legacy pair is
			// synthesized — the sent id stays explicit rather than implied.
			name:  "deprecated speed tiers resolve to the legacy pair",
			model: codexModel{AdditionalSpeedTiers: []string{"fast"}},
			want:  &provider.FastModeTier{ID: "priority", Name: "Fast"},
		},
		{
			name: "canonical serviceTiers take precedence over the deprecated key",
			model: codexModel{
				ServiceTiers:         []codexModelServiceTier{{ID: "flex", Name: "Flex"}},
				AdditionalSpeedTiers: []string{"fast"},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexFastModeTier(tt.model)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("codexFastModeTier = %#v, want nil", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("codexFastModeTier = nil, want %#v", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Fatalf("codexFastModeTier = %#v, want %#v", *got, *tt.want)
			}

			// Support and tier are one answer: mapCodexModel must never
			// advertise the capability without an id to send with it.
			mapped := mapCodexModel(codexModel{Model: "gpt-test", ServiceTiers: tt.model.ServiceTiers, AdditionalSpeedTiers: tt.model.AdditionalSpeedTiers})
			if contains(mapped.Capabilities, provider.ModelCapabilityFastMode) != (tt.want != nil) {
				t.Errorf("Capabilities = %#v, want fast-mode marker = %v", mapped.Capabilities, tt.want != nil)
			}
		})
	}
}

func TestNormalizeCodexDisplayNameUsesFriendlyGPTAliases(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5":             "GPT 5.5",
		"GPT-5.6-Sol":         "GPT 5.6 Sol",
		"GPT-5.4 Mini":        "GPT 5.4 Mini",
		"gpt-5.3-codex-spark": "GPT 5.3 Codex Spark",
		"o4-mini":             "o4-mini",
		"GPTurbo":             "GPTurbo",
	}
	for input, want := range cases {
		if got := normalizeCodexDisplayName(input); got != want {
			t.Errorf("normalizeCodexDisplayName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestListModelsFollowsPagination(t *testing.T) {
	binary := writeModelListPagingFakeCodex(t)

	models, err := ListModels(context.Background(), ModelListConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2: %#v", len(models), models)
	}
	if models[0].Slug != "gpt-5.4" || models[1].Slug != "gpt-5.4-mini" {
		t.Fatalf("models = %#v, want paged gpt-5.4 then gpt-5.4-mini", models)
	}
	if len(models[0].ContextWindows) != 2 ||
		models[0].ContextWindows[0].Tokens != provider.CodexStandardContextWindow ||
		models[0].ContextWindows[1].Tokens != provider.CodexExtendedContextWindow {
		t.Fatalf("gpt-5.4 ContextWindows = %#v, want codex standard + extended", models[0].ContextWindows)
	}
}

func TestListModelsRejectsRepeatedCursor(t *testing.T) {
	binary := writeModelListRepeatedCursorFakeCodex(t)

	_, err := ListModels(context.Background(), ModelListConfig{Binary: binary})
	if err == nil || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("ListModels repeated cursor error = %v, want repeated cursor", err)
	}
}

func TestListModelsRejectsExcessivePages(t *testing.T) {
	binary := writeModelListEndlessPagingFakeCodex(t)

	_, err := ListModels(context.Background(), ModelListConfig{Binary: binary})
	if err == nil || !strings.Contains(err.Error(), "exceeded 20 pages") {
		t.Fatalf("ListModels excessive pages error = %v, want exceeded pages", err)
	}
}

func writeModelListFakeCodex(t *testing.T, modelsJSON string) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":%s,"nextCursor":null}}\n' "$id" '` + modelsJSON + `'
  fi
done
`
	return writeExecutable(t, script)
}

func writeModelListPagingFakeCodex(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"cursor":"1"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.4-mini","displayName":"GPT-5.4 Mini","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","serviceTiers":[]}],"nextCursor":null}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.4","displayName":"GPT-5.4","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","serviceTiers":[{"id":"priority","name":"Fast","description":"1.5x speed, increased usage"}]}],"nextCursor":"1"}}\n' "$id"
  fi
done
`
	return writeExecutable(t, script)
}

func writeModelListRepeatedCursorFakeCodex(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[],"nextCursor":"loop"}}\n' "$id"
  fi
done
`
	return writeExecutable(t, script)
}

func writeModelListEndlessPagingFakeCodex(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[],"nextCursor":"cursor-%s"}}\n' "$id" "$id"
  fi
done
`
	return writeExecutable(t, script)
}

func writeExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
