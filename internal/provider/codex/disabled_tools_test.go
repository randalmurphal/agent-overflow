package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// The table is the whole feature: a wrong or missing key leaves a tool the
// user disabled in the model's context. Verified against rust-v0.147.0 —
// docs/references/codex-instructions-tools.md carries the source citations.
func TestDisabledToolConfigOverridesMapsEveryToggle(t *testing.T) {
	want := map[string]map[string]any{
		"web_search":         {"web_search": "disabled"},
		"update_plan":        {"tools.update_plan.enabled": false},
		"view_image":         {"features.view_image": false},
		"request_user_input": {"tools.experimental_request_user_input.enabled": false},
		"collab_agents": {
			"agents.enabled":          false,
			"features.multi_agent_v2": false,
			"features.multi_agent":    false,
		},
		"image_generation": {"features.image_generation": false},
		"tool_suggest":     {"features.tool_suggest": false},
	}
	for _, id := range DisabledToolToggleIDs() {
		expected, ok := want[id]
		if !ok {
			t.Fatalf("toggle %q has no asserted config keys — add them here and confirm them in codex-source", id)
		}
		got := DisabledToolConfigOverrides([]string{id})
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("DisabledToolConfigOverrides(%q) = %v, want %v", id, got, expected)
		}
	}
	if len(want) != len(DisabledToolToggleIDs()) {
		t.Fatalf("asserted %d toggles but the table has %d", len(want), len(DisabledToolToggleIDs()))
	}
}

func TestDisabledToolConfigOverridesMergesAndIgnoresUnknownIDs(t *testing.T) {
	// An id this build does not know is skipped, never fatal: the list is
	// settings data that outlives any one AO version.
	got := DisabledToolConfigOverrides([]string{"web_search", "  ", "not_a_real_toggle", "view_image"})
	want := map[string]any{
		"web_search":          "disabled",
		"features.view_image": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DisabledToolConfigOverrides() = %v, want %v", got, want)
	}
	if DisabledToolConfigOverrides(nil) != nil {
		t.Fatal("empty list must produce no overrides")
	}
	if DisabledToolConfigOverrides([]string{"not_a_real_toggle"}) != nil {
		t.Fatal("an all-unknown list must produce no overrides, not an empty map")
	}
}

func TestBuildThreadParamsCarriesDisabledToolConfigKeys(t *testing.T) {
	params := buildThreadParams(Config{
		DisabledTools:   []string{"update_plan", "collab_agents"},
		ReasoningEffort: "high",
	}, "")
	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("params[config] = %T, want map", params["config"])
	}
	// Dotted keys stay flat: codex expands them into nested TOML itself
	// (config/src/overrides.rs).
	for key, want := range map[string]any{
		"tools.update_plan.enabled": false,
		"agents.enabled":            false,
		"features.multi_agent_v2":   false,
		"features.multi_agent":      false,
		"model_reasoning_effort":    "high",
	} {
		if got, present := config[key]; !present || got != want {
			t.Errorf("config[%q] = %v (present=%v), want %v", key, got, present, want)
		}
	}
}

func TestConfigFromOptionsCarriesDisabledTools(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "codex",
		DisabledTools: []string{"web_search"},
	})
	if !reflect.DeepEqual(cfg.DisabledTools, []string{"web_search"}) {
		t.Fatalf("cfg.DisabledTools = %v", cfg.DisabledTools)
	}
}

// The config map and baseInstructions are start-time only — Codex re-reads
// neither per turn — so a change in either must fall out as a restart.
func TestPlanLiveUpdateRequiresRestartForOverrideAxes(t *testing.T) {
	base := provider.SessionOptions{Provider: "codex", Model: "gpt-5.6-sol"}

	withTools := base
	withTools.DisabledTools = []string{"web_search"}
	if _, ok := PlanLiveUpdate(base, withTools); ok {
		t.Error("PlanLiveUpdate() ok = true for a disabled-tool change; the config map is start-only")
	}

	withPrompt := base
	withPrompt.SystemPrompt = "replacement"
	if _, ok := PlanLiveUpdate(base, withPrompt); ok {
		t.Error("PlanLiveUpdate() ok = true for a baseInstructions change; it is start-only")
	}
}

// A cold thread/resume that omits baseInstructions inherits whatever the
// thread was FIRST started with (session/mod.rs resolution priority), and
// one that omits the config map rebuilds the thread with the disabled tools
// back in the request. Both methods therefore have to carry them — which
// they do because they share buildThreadParams. This drives a real
// (mock-backed) session so a future split of that call site fails here.
func TestThreadStartAndResumeBothCarryOverrideAxes(t *testing.T) {
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	script := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + requestLog + "'\n" +
		"  id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"  if [ -z \"$id\" ]; then continue; fi\n" +
		"  printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"mock-thread\"}}}\\n' \"$id\"\n" +
		"done\n"
	binary := filepath.Join(dir, "codex")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock app-server: %v", err)
	}

	cfg := Config{
		Binary:        binary,
		Model:         "gpt-5.6-sol",
		SystemPrompt:  "replacement instructions",
		DisabledTools: []string{"web_search"},
	}

	for _, tc := range []struct {
		name     string
		method   string
		resumeID string
	}{
		{name: "start", method: "thread/start"},
		{name: "resume", method: "thread/resume", resumeID: "mock-thread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(requestLog, nil, 0o644); err != nil {
				t.Fatalf("reset request log: %v", err)
			}
			runCfg := cfg
			runCfg.ResumeThreadID = tc.resumeID
			sess, err := NewSession(context.Background(), "thread-1", runCfg, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession() error = %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })

			params := findRequestParams(t, requestLog, tc.method)
			if got := params["baseInstructions"]; got != "replacement instructions" {
				t.Errorf("%s baseInstructions = %v, want the override text", tc.method, got)
			}
			config, ok := params["config"].(map[string]any)
			if !ok {
				t.Fatalf("%s params[config] = %T, want map", tc.method, params["config"])
			}
			if got := config["web_search"]; got != "disabled" {
				t.Errorf("%s config[web_search] = %v, want disabled", tc.method, got)
			}
		})
	}
}

// The settings UI renders its Codex toggles from a hand-mirrored copy of the
// id set this package owns, and the two cannot be one table: the frontend
// needs labels and copy Go has no business holding, and Go's ids are config
// keys the frontend has no business holding. What they must agree on is the
// ID SET — an id only the frontend knows renders a switch that disables
// nothing (the spawn path skips it with a log line the user never sees), and
// an id only Go knows is a capability the settings UI cannot turn off at
// all. Same two-sided-copy pin as TestReservedEnvNamesMatchTheProviderPins,
// parsing the mirror the way internal/highlight pins its JS hash parity.
func TestDisabledToolTogglesMatchTheFrontendMirror(t *testing.T) {
	const mirror = "frontend/src/lib/utils/promptOverrides.ts"
	source := readRepoFile(t, mirror)
	frontend := setOf(exportedArrayLiterals(t, source, mirror, "CODEX_TOOL_TOGGLES", `\bid:\s*'([^']*)'`))
	owned := setOf(DisabledToolToggleIDs())

	for id := range owned {
		if _, ok := frontend[id]; !ok {
			t.Errorf("disabled_tools.go knows toggle %q but %s does not offer it", id, mirror)
		}
	}
	for id := range frontend {
		if _, ok := owned[id]; !ok {
			t.Errorf("%s offers toggle %q but disabled_tools.go maps it to no config keys", mirror, id)
		}
	}
}

func setOf(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// readRepoFile reads a repo-relative path. The repository root is derived
// from this source file's own location rather than the working directory,
// which `go test` sets to the package directory.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the repository root: runtime.Caller failed")
	}
	// internal/provider/codex/<this file> → repository root.
	root := filepath.Join(filepath.Dir(sourcePath), "..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// exportedArrayLiterals pulls every capture of pattern out of the
// `export const <name> = [ … ]` block of a TypeScript source. Anchoring on
// the const name and on the flush-left `]` that closes it keeps comments,
// blank lines and reformatting inside the array harmless; the file carries
// a comment saying a Go test parses it. Extracting nothing is a hard
// failure — a silently empty set would pin nothing while looking green.
func exportedArrayLiterals(t *testing.T, source, path, name, pattern string) []string {
	t.Helper()

	start := strings.Index(source, "export const "+name)
	if start < 0 {
		t.Fatalf("%s: no `export const %s` — this pin cannot find its mirror", path, name)
	}
	block := source[start:]
	open := strings.Index(block, "[")
	end := strings.Index(block, "\n]")
	if open < 0 || end < open {
		t.Fatalf("%s: `export const %s` is not a bracketed array literal", path, name)
	}
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(block[open:end], -1)
	if len(matches) == 0 {
		t.Fatalf("%s: %s matched no %s literals — the entry syntax the pin parses changed", path, name, pattern)
	}
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
}

func findRequestParams(t *testing.T, logPath, method string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.Method == method {
			return req.Params
		}
	}
	t.Fatalf("no %s request in:\n%s", method, data)
	return nil
}
