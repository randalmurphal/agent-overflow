package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func thinkingOptions(mode, display string, budget int) provider.SessionOptions {
	opts := liveUpdateBaseOptions()
	opts.ClaudeThinking = provider.ClaudeThinking{Mode: mode, BudgetTokens: budget, Display: display}
	return opts
}

// TestPlanLiveUpdateThinkingTransitions covers the TRANSITIONS, not the
// states. Like the system-prompt axis this one is ASYMMETRIC, and the
// asymmetry is the whole risk: `set_max_thinking_tokens` can disable
// thinking or pin a budget, but `max_thinking_tokens: null` is accepted and
// does NOTHING (spike-verified 2.1.237), so there is no request that
// restores the CLI's own choice.
//
// Getting that wrong in the permissive direction is silent: the CLI would
// ack, the session would keep the old budget, and launchOpts would claim
// the default.
func TestPlanLiveUpdateThinkingTransitions(t *testing.T) {
	tests := []struct {
		name       string
		prev       provider.SessionOptions
		next       provider.SessionOptions
		wantOK     bool
		wantUpdate ThinkingUpdate
	}{
		{
			name:   "default to budget",
			prev:   thinkingOptions("", "", 0),
			next:   thinkingOptions("budget", "", 2048),
			wantOK: true,
			wantUpdate: ThinkingUpdate{
				Apply: true, SendBudget: true, Budget: 2048, Display: ThinkingDisplaySummarized,
			},
		},
		{
			name:       "budget to off: zero disables, and display is dropped with it",
			prev:       thinkingOptions("budget", "", 2048),
			next:       thinkingOptions("off", "", 0),
			wantOK:     true,
			wantUpdate: ThinkingUpdate{Apply: true, SendBudget: true, Budget: 0},
		},
		{
			name:   "off to budget: display is re-asserted, not diffed",
			prev:   thinkingOptions("off", "", 0),
			next:   thinkingOptions("budget", "omitted", 8000),
			wantOK: true,
			wantUpdate: ThinkingUpdate{
				Apply: true, SendBudget: true, Budget: 8000, Display: ThinkingDisplayOmitted,
			},
		},
		{
			name:   "budget edited: one request carries the new number",
			prev:   thinkingOptions("budget", "", 2048),
			next:   thinkingOptions("budget", "", 16000),
			wantOK: true,
			wantUpdate: ThinkingUpdate{
				Apply: true, SendBudget: true, Budget: 16000, Display: ThinkingDisplaySummarized,
			},
		},
		{
			name:   "budget to default: no revert-to-default wire form",
			prev:   thinkingOptions("budget", "", 2048),
			next:   thinkingOptions("", "", 0),
			wantOK: false,
		},
		{
			name:   "off to default: same hole",
			prev:   thinkingOptions("off", "", 0),
			next:   thinkingOptions("", "", 0),
			wantOK: false,
		},
		{
			name:       "display only: thinking_display alone is an accepted request",
			prev:       thinkingOptions("", "summarized", 0),
			next:       thinkingOptions("", "omitted", 0),
			wantOK:     true,
			wantUpdate: ThinkingUpdate{Apply: true, Display: ThinkingDisplayOmitted},
		},
		{
			name:   "unchanged carries nothing",
			prev:   thinkingOptions("budget", "omitted", 2048),
			next:   thinkingOptions("budget", "omitted", 2048),
			wantOK: true,
		},
		{
			// A disabled session has no display, so the two configs are
			// the same session and must not buy a control request.
			name:   "display change while off is inert",
			prev:   thinkingOptions("off", "summarized", 0),
			next:   thinkingOptions("off", "omitted", 0),
			wantOK: true,
		},
		{
			// "summarized" IS the default AO spawns, so naming it
			// explicitly must not look like a change.
			name:   "explicit summarized equals the unset default",
			prev:   thinkingOptions("", "", 0),
			next:   thinkingOptions("", "summarized", 0),
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			update, ok := PlanLiveUpdate(tc.prev, tc.next)
			if ok != tc.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v (update %+v)", ok, tc.wantOK, update)
			}
			if !ok {
				return
			}
			if update.Thinking != tc.wantUpdate {
				t.Fatalf("update.Thinking = %+v, want %+v", update.Thinking, tc.wantUpdate)
			}
			// Nothing else may ride along: the thinking axis has its own
			// request and must not, for instance, re-send the model.
			want := LiveUpdate{Thinking: tc.wantUpdate}
			if update != want {
				t.Fatalf("update = %+v, want %+v", update, want)
			}
		})
	}
}

// The thinking axis composes with the others rather than replacing them:
// one reconcile can legitimately carry a model swap and a budget change,
// and they travel as two separate control requests.
func TestPlanLiveUpdateThinkingComposesWithModel(t *testing.T) {
	prev := thinkingOptions("", "", 0)
	next := thinkingOptions("budget", "", 4096)
	next.Model = "claude-fable-5"

	update, ok := PlanLiveUpdate(prev, next)
	if !ok {
		t.Fatal("PlanLiveUpdate refused a model + thinking change")
	}
	want := LiveUpdate{
		Model: "claude-fable-5",
		Thinking: ThinkingUpdate{
			Apply: true, SendBudget: true, Budget: 4096, Display: ThinkingDisplaySummarized,
		},
	}
	if update != want {
		t.Fatalf("update = %+v, want %+v", update, want)
	}
}

func capturedThinkingRequest(t *testing.T, line string) (subtype string, raw map[string]json.RawMessage) {
	t.Helper()
	var captured struct {
		Type    string                     `json:"type"`
		Request map[string]json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" {
		t.Fatalf("captured type = %q, want control_request", captured.Type)
	}
	if err := json.Unmarshal(captured.Request["subtype"], &subtype); err != nil {
		t.Fatalf("decode subtype: %v", err)
	}
	return subtype, captured.Request
}

// The wire shape per mode. The OMISSIONS are the contract: `null` is a
// no-op, so "leave the budget alone" has to be the absent key, and a
// disabling request must not carry a display the CLI would drop.
func TestApplyLiveUpdateThinkingWireShape(t *testing.T) {
	tests := []struct {
		name        string
		update      ThinkingUpdate
		wantKeys    map[string]string
		absentKeys  []string
		wantSubtype string
	}{
		{
			name:        "budget with display",
			update:      ThinkingUpdate{Apply: true, SendBudget: true, Budget: 2048, Display: ThinkingDisplayOmitted},
			wantSubtype: "set_max_thinking_tokens",
			wantKeys:    map[string]string{"max_thinking_tokens": "2048", "thinking_display": `"omitted"`},
		},
		{
			name:        "off sends a bare zero",
			update:      ThinkingUpdate{Apply: true, SendBudget: true, Budget: 0},
			wantSubtype: "set_max_thinking_tokens",
			wantKeys:    map[string]string{"max_thinking_tokens": "0"},
			absentKeys:  []string{"thinking_display"},
		},
		{
			name:        "display only omits the budget key entirely",
			update:      ThinkingUpdate{Apply: true, Display: ThinkingDisplaySummarized},
			wantSubtype: "set_max_thinking_tokens",
			wantKeys:    map[string]string{"thinking_display": `"summarized"`},
			absentKeys:  []string{"max_thinking_tokens"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, capturePath := liveUpdateSessionAtVersion(t, Config{BasePermissionMode: "default"}, "2.1.237")

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Thinking: tc.update}, nil); err != nil {
				t.Fatalf("ApplyLiveUpdate: %v", err)
			}

			lines := waitCapturedLines(t, capturePath, 1)
			subtype, request := capturedThinkingRequest(t, lines[0])
			if subtype != tc.wantSubtype {
				t.Fatalf("subtype = %q, want %q", subtype, tc.wantSubtype)
			}
			for key, want := range tc.wantKeys {
				got, present := request[key]
				if !present {
					t.Fatalf("request missing %q: %v", key, request)
				}
				if string(got) != want {
					t.Fatalf("request[%q] = %s, want %s", key, got, want)
				}
			}
			for _, key := range tc.absentKeys {
				if _, present := request[key]; present {
					t.Fatalf("request carries %q, which must be omitted: %v", key, request)
				}
			}
		})
	}
}

// A CLI too old to carry the handler — including one that has not reported
// a version yet — takes the restart path, and does so BEFORE any wire
// write, so a restart-bound update never half-applies.
func TestApplyLiveUpdateThinkingRequiresVersionFloor(t *testing.T) {
	for _, version := range []string{"", "2.1.213"} {
		s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
		if version != "" {
			s.noteCLIVersion(version)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		update := LiveUpdate{
			Model:    "claude-fable-5",
			Thinking: ThinkingUpdate{Apply: true, SendBudget: true, Budget: 2048},
		}
		_, err := s.ApplyLiveUpdate(ctx, update, nil)
		cancel()
		if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
			t.Fatalf("version %q: ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", version, err)
		}
		time.Sleep(50 * time.Millisecond)
		if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
			t.Fatalf("version %q: expected no stdin writes, captured: %s", version, data)
		}
	}
}

// A malformed axis is refused as an error rather than sent: the handler
// would reject it, and a rejection arriving as thread error state for a
// request AO should never have made is worse than a local refusal.
func TestApplyLiveUpdateThinkingRejectsMalformedAxis(t *testing.T) {
	tests := []struct {
		name   string
		update ThinkingUpdate
	}{
		{"negative budget", ThinkingUpdate{Apply: true, SendBudget: true, Budget: -1}},
		{"unknown display", ThinkingUpdate{Apply: true, Display: "hidden"}},
		{"apply with nothing to send", ThinkingUpdate{Apply: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, capturePath := liveUpdateSessionAtVersion(t, Config{BasePermissionMode: "default"}, "2.1.237")

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Thinking: tc.update}, nil)
			if err == nil || errors.Is(err, ErrLiveUpdateRequiresRestart) {
				t.Fatalf("ApplyLiveUpdate error = %v, want a validation failure", err)
			}
			time.Sleep(50 * time.Millisecond)
			if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
				t.Fatalf("expected no stdin writes, captured: %s", data)
			}
		})
	}
}

// The spawn half. `--thinking enabled` means ADAPTIVE on the CLI side, so a
// fixed budget has exactly one flag; `--thinking` outranks
// `--max-thinking-tokens`, so the two are never sent together.
func TestBuildArgsThinkingSpawnForm(t *testing.T) {
	tests := []struct {
		name     string
		thinking provider.ClaudeThinking
		want     []string
	}{
		{
			name: "unset keeps the always-summarized default",
			want: []string{"--thinking-display", "summarized"},
		},
		{
			name:     "off disables and carries no display",
			thinking: provider.ClaudeThinking{Mode: "off", Display: "omitted"},
			want:     []string{"--thinking", "disabled"},
		},
		{
			name:     "budget uses --max-thinking-tokens, never --thinking",
			thinking: provider.ClaudeThinking{Mode: "budget", BudgetTokens: 2048, Display: "omitted"},
			want:     []string{"--max-thinking-tokens", "2048", "--thinking-display", "omitted"},
		},
		{
			name:     "display alone",
			thinking: provider.ClaudeThinking{Display: "omitted"},
			want:     []string{"--thinking-display", "omitted"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := liveUpdateBaseOptions()
			opts.ClaudeThinking = tc.thinking
			args := buildArgs(ConfigFromOptions(opts), "")
			if !hasArgSequence(args, tc.want) {
				t.Fatalf("args %v missing sequence %v", args, tc.want)
			}
			if tc.thinking.Mode == "budget" && hasArg(args, "--thinking") {
				t.Fatalf("args %v carry --thinking beside a fixed budget", args)
			}
			if tc.thinking.Mode == "off" && hasArg(args, "--thinking-display") {
				t.Fatalf("args %v carry --thinking-display on a disabled session", args)
			}
		})
	}
}

// A budget mode with no usable budget must never render
// `--max-thinking-tokens 0`, which the CLI reads as DISABLED — the opposite
// of an unfinished budget's meaning. Settings refuses that shape first;
// this is the second wall, for a caller that builds SessionOptions itself.
func TestConfigFromOptionsRefusesZeroBudget(t *testing.T) {
	opts := liveUpdateBaseOptions()
	opts.ClaudeThinking = provider.ClaudeThinking{Mode: "budget"}
	cfg := ConfigFromOptions(opts)
	if cfg.Thinking.Mode != ThinkingDefault || cfg.Thinking.BudgetTokens != 0 {
		t.Fatalf("Thinking = %+v, want the default mode", cfg.Thinking)
	}
	if hasArg(buildArgs(cfg, ""), "--max-thinking-tokens") {
		t.Fatal("a budget-less budget mode rendered --max-thinking-tokens")
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func hasArgSequence(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j, w := range want {
			if args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
