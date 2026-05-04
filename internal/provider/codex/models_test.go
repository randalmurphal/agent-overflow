package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestListModelsUsesCodexModelListMetadata(t *testing.T) {
	binary := writeModelListFakeCodex(t, `[{"model":"gpt-5.5","displayName":"gpt-5.5","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Fast responses with lighter reasoning"},{"reasoningEffort":"high","description":"Greater reasoning depth for complex problems"},{"reasoningEffort":"xhigh","description":"Extra high reasoning depth for complex problems"}],"defaultReasoningEffort":"high","additionalSpeedTiers":["fast"]},{"model":"legacy-hidden","displayName":"Legacy Hidden","hidden":true,"supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Low"}],"defaultReasoningEffort":"low","additionalSpeedTiers":[]}]`)

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
	if model.Name != "GPT-5.5" {
		t.Errorf("Name = %q, want GPT-5.5", model.Name)
	}
	if !contains(model.Capabilities, provider.ModelCapabilityFastMode) {
		t.Errorf("Capabilities = %#v, want fast mode", model.Capabilities)
	}
	if len(model.ContextWindows) != 2 ||
		model.ContextWindows[0].Tokens != provider.CodexStandardContextWindow ||
		model.ContextWindows[1].Tokens != provider.CodexExtendedContextWindow {
		t.Fatalf("ContextWindows = %#v, want codex standard + extended", model.ContextWindows)
	}
	if len(model.ReasoningEfforts) != 3 {
		t.Fatalf("ReasoningEfforts len = %d, want 3", len(model.ReasoningEfforts))
	}
	if model.ReasoningEfforts[1].Slug != "high" || !model.ReasoningEfforts[1].Default {
		t.Errorf("ReasoningEfforts[1] = %#v, want default high", model.ReasoningEfforts[1])
	}
	if model.ReasoningEfforts[0].Label != "Low" ||
		model.ReasoningEfforts[1].Label != "High" ||
		model.ReasoningEfforts[2].Label != "Extra High" {
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
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.4-mini","displayName":"GPT-5.4 Mini","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","additionalSpeedTiers":[]}],"nextCursor":null}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.4","displayName":"GPT-5.4","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","additionalSpeedTiers":["fast"]}],"nextCursor":"1"}}\n' "$id"
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
