package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func codexLiveUpdateBaseOptions() provider.SessionOptions {
	return provider.SessionOptions{
		Provider:        string(provider.Codex),
		Model:           "gpt-5.5-codex",
		WorkDir:         "/tmp/work",
		ReasoningEffort: provider.EffortHigh,
		Mode:            provider.ModeChat,
		RuntimeMode:     provider.RuntimeApprovalRequired,
		SystemPrompt:    "base prompt",
		Resume:          "thread-ref-1",
	}
}

func TestCodexPlanLiveUpdate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*provider.SessionOptions)
		wantOK bool
		check  func(t *testing.T, u LiveUpdate)
	}{
		{
			name:   "identical options are live",
			mutate: func(*provider.SessionOptions) {},
			wantOK: true,
		},
		{
			name:   "model change is live",
			mutate: func(o *provider.SessionOptions) { o.Model = "gpt-5.6-codex" },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.Model != "gpt-5.6-codex" {
					t.Fatalf("update.Model = %q, want gpt-5.6-codex", u.Model)
				}
			},
		},
		{
			name:   "effort change is live",
			mutate: func(o *provider.SessionOptions) { o.ReasoningEffort = provider.EffortLow },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ReasoningEffort != "low" {
					t.Fatalf("update.ReasoningEffort = %q, want low", u.ReasoningEffort)
				}
			},
		},
		{
			name:   "fast mode change is live",
			mutate: func(o *provider.SessionOptions) { o.FastMode = true },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ServiceTier != fastServiceTier {
					t.Fatalf("update.ServiceTier = %q, want %q", u.ServiceTier, fastServiceTier)
				}
			},
		},
		{
			// The tier id is resolved from the live catalog, so it can change
			// under a thread that never touched its fast-mode toggle (a catalog
			// refresh, a model switch). That must plan live, and must plan the
			// NEW id — it is a turn/start override like model and effort.
			name: "fast mode with a wire-driven tier id is live",
			mutate: func(o *provider.SessionOptions) {
				o.FastMode = true
				o.FastModeTierID = "turbo"
			},
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ServiceTier != "turbo" {
					t.Fatalf("update.ServiceTier = %q, want turbo", u.ServiceTier)
				}
			},
		},
		{
			name:   "runtime mode change is live",
			mutate: func(o *provider.SessionOptions) { o.RuntimeMode = provider.RuntimeFullAccess },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ApprovalPolicy != "never" || u.Sandbox != "danger-full-access" {
					t.Fatalf("update approval/sandbox = %q/%q, want never/danger-full-access", u.ApprovalPolicy, u.Sandbox)
				}
			},
		},
		{
			name:   "resume cursor drift is lifecycle, not config",
			mutate: func(o *provider.SessionOptions) { o.Resume = "thread-ref-2" },
			wantOK: true,
		},
		{
			name:   "context window change needs restart",
			mutate: func(o *provider.SessionOptions) { o.ContextWindow = 200000 },
			wantOK: false,
		},
		{
			name: "autocompact change needs restart",
			mutate: func(o *provider.SessionOptions) {
				o.AutoCompactPercent = 80
			},
			wantOK: false,
		},
		{
			name:   "system prompt change needs restart",
			mutate: func(o *provider.SessionOptions) { o.SystemPrompt = "different prompt" },
			wantOK: false,
		},
		{
			name:   "workdir change needs restart",
			mutate: func(o *provider.SessionOptions) { o.WorkDir = "/tmp/elsewhere" },
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := codexLiveUpdateBaseOptions()
			next := codexLiveUpdateBaseOptions()
			tt.mutate(&next)

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tt.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if (update != LiveUpdate{}) {
					t.Fatalf("PlanLiveUpdate update = %+v, want zero when restart is required", update)
				}
				return
			}
			if tt.check != nil {
				tt.check(t, update)
			}
		})
	}
}

// TestCodexPlanLiveUpdateFastModeTransitions covers the fast-mode axis as a
// sequence rather than a set of states, with a wire-driven tier id in play. The
// off edge is the one a states-only test misses: an update that simply omitted
// ServiceTier when fast mode went off would leave the session's turn config
// carrying the tier forever, and every subsequent turn would still be billed as
// fast. Tier-id drift under a thread that never touched its own toggle (a
// catalog refresh renaming the tier) has to plan live too — it is a turn/start
// override like model and effort, not a spawn-only axis.
func TestCodexPlanLiveUpdateFastModeTransitions(t *testing.T) {
	fastOff := codexLiveUpdateBaseOptions()
	fastOff.FastModeTierID = "turbo"

	fastOn := fastOff
	fastOn.FastMode = true

	renamed := fastOn
	renamed.FastModeTierID = "priority"

	tests := []struct {
		name string
		prev provider.SessionOptions
		next provider.SessionOptions
		want string
	}{
		{name: "off to on sends the wire tier", prev: fastOff, next: fastOn, want: "turbo"},
		{name: "on to off clears the tier", prev: fastOn, next: fastOff, want: ""},
		{name: "tier id drift alone re-sends", prev: fastOn, next: renamed, want: "priority"},
		{name: "on to on unchanged is stable", prev: fastOn, next: fastOn, want: "turbo"},
	}

	// One session carried across the whole sequence: ApplyLiveUpdate is
	// persistent side-state, so what matters is that each step leaves the turn
	// config correct given everything before it.
	s := &Session{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			update, ok := PlanLiveUpdate(tt.prev, tt.next)
			if !ok {
				t.Fatalf("PlanLiveUpdate needs a restart; serviceTier is a turn/start override")
			}
			if update.ServiceTier != tt.want {
				t.Fatalf("update.ServiceTier = %q, want %q", update.ServiceTier, tt.want)
			}
			s.ApplyLiveUpdate(update)
			if got := s.turnConfig().ServiceTier; got != tt.want {
				t.Fatalf("turnConfig().ServiceTier = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyLiveUpdateFastModeToggleReachesTurnStart proves the transition at the
// wire, not just in session state: a wire-driven tier id lands as `serviceTier`
// on the next turn/start, and turning fast mode off omits the key entirely on
// the turn after that. Codex reads an absent key as "inherit the thread
// default", so sending a stale id and sending nothing are materially different
// requests.
func TestApplyLiveUpdateFastModeToggleReachesTurnStart(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(codexTurnCaptureScript(capturePath)), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:         scriptPath,
		Model:          "gpt-5.5",
		WorkDir:        "/tmp",
		ApprovalPolicy: "untrusted",
		Sandbox:        "read-only",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	fastOff := codexLiveUpdateBaseOptions()
	fastOff.FastModeTierID = "turbo"
	fastOn := fastOff
	fastOn.FastMode = true

	sendWith := func(prev, next provider.SessionOptions) {
		t.Helper()
		update, ok := PlanLiveUpdate(prev, next)
		if !ok {
			t.Fatalf("PlanLiveUpdate needs a restart")
		}
		s.ApplyLiveUpdate(update)
		if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	sendWith(fastOff, fastOn)
	sendWith(fastOn, fastOff)
	_ = s.Close()

	turns := capturedTurnStartParams(t, capturePath)
	if len(turns) != 2 {
		t.Fatalf("captured %d turn/start requests, want 2", len(turns))
	}
	if turns[0]["serviceTier"] != "turbo" {
		t.Errorf("first turn serviceTier = %v, want turbo", turns[0]["serviceTier"])
	}
	if _, present := turns[1]["serviceTier"]; present {
		t.Errorf("second turn carries serviceTier = %v, want the key omitted after fast mode off", turns[1]["serviceTier"])
	}
}

// codexTurnCaptureScript is a stdin-logging app-server stub: it answers every
// request with the minimal successful shape and appends the raw line to path so
// a test can assert on what actually went out.
func codexTurnCaptureScript(path string) string {
	return fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-1\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread\"}}}"
    fi
done
`, path)
}

// capturedTurnStartParams returns the params bag of every turn/start request in
// a captured stdin log, in order.
func capturedTurnStartParams(t *testing.T, path string) []map[string]any {
	t.Helper()
	captured, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var params []map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/start"`) {
			continue
		}
		var request map[string]any
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatalf("unmarshal turn/start: %v", err)
		}
		bag, ok := request["params"].(map[string]any)
		if !ok {
			t.Fatalf("turn/start carried no params object: %s", line)
		}
		params = append(params, bag)
	}
	return params
}

// TestApplyLiveUpdateAppliesOnNextTurnStart pins the end-to-end behavior a
// mid-session config change depends on: ApplyLiveUpdate swaps the session's
// turn config and the very next Send carries the new model / effort /
// serviceTier / approvalPolicy / sandboxPolicy as turn/start overrides —
// no restart, same thread.
func TestApplyLiveUpdateAppliesOnNextTurnStart(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(codexTurnCaptureScript(capturePath)), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:          scriptPath,
		Model:           "gpt-5.5-codex",
		WorkDir:         "/tmp",
		ReasoningEffort: "high",
		ApprovalPolicy:  "untrusted",
		Sandbox:         "read-only",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.ApplyLiveUpdate(LiveUpdate{
		Model:           "gpt-5.6-codex",
		ReasoningEffort: "low",
		ServiceTier:     fastServiceTier,
		ApprovalPolicy:  "never",
		Sandbox:         "danger-full-access",
	})

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = s.Close()

	turns := capturedTurnStartParams(t, capturePath)
	if len(turns) == 0 {
		t.Fatal("captured no turn/start request")
	}

	params := turns[0]
	if params["model"] != "gpt-5.6-codex" {
		t.Fatalf("model = %v, want gpt-5.6-codex", params["model"])
	}
	if params["effort"] != "low" {
		t.Fatalf("effort = %v, want low", params["effort"])
	}
	if params["serviceTier"] != fastServiceTier {
		t.Fatalf("serviceTier = %v, want %q", params["serviceTier"], fastServiceTier)
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %v, want never", params["approvalPolicy"])
	}
	sandboxPolicy, ok := params["sandboxPolicy"].(map[string]any)
	if !ok || sandboxPolicy["type"] != "dangerFullAccess" {
		t.Fatalf("sandboxPolicy = %v, want dangerFullAccess", params["sandboxPolicy"])
	}
	// The collaboration-mode settings bag must reflect the new config too —
	// it carries model + reasoning_effort on every turn.
	collab := params["collaborationMode"].(map[string]any)
	settings := collab["settings"].(map[string]any)
	if settings["model"] != "gpt-5.6-codex" || settings["reasoning_effort"] != "low" {
		t.Fatalf("collaborationMode settings = %v, want new model/effort", settings)
	}

	if got := s.currentModel(); got != "gpt-5.6-codex" {
		t.Fatalf("currentModel = %q, want gpt-5.6-codex", got)
	}
}

// TestCodexPlanLiveUpdateRuntimeModeTransitions covers transitions rather than
// resting states, across EVERY ordered pair of tiers. Codex has no spawn-only
// runtime-mode axis — approvalPolicy, sandboxPolicy and approvalsReviewer are
// all turn/start overrides upstream documents as applying "for this turn and
// subsequent turns" — so every pair must plan live, including the pairs that
// force a restart on Claude (anything touching read-only, whose
// `--disallowedTools` is spawn-only there).
//
// The reviewer assertion is the transition-specific half: it is not enough
// that a switch INTO auto sets auto_review, the switch back OUT has to set
// user. An update that simply omitted the reviewer would pass a states-only
// test and leave the thread auto-reviewing forever.
func TestCodexPlanLiveUpdateRuntimeModeTransitions(t *testing.T) {
	for _, from := range provider.AllRuntimeModes {
		for _, to := range provider.AllRuntimeModes {
			t.Run(string(from)+" to "+string(to), func(t *testing.T) {
				prev := codexLiveUpdateBaseOptions()
				prev.RuntimeMode = from
				next := prev
				next.RuntimeMode = to

				update, ok := PlanLiveUpdate(prev, next)
				if !ok {
					t.Fatalf("PlanLiveUpdate(%q → %q) needs a restart; every Codex runtime axis is a turn/start override", from, to)
				}
				want := wantCodexRuntime[to]
				got := codexRuntime{
					ApprovalPolicy:    update.ApprovalPolicy,
					Sandbox:           update.Sandbox,
					ApprovalsReviewer: update.ApprovalsReviewer,
				}
				if got != want {
					t.Errorf("update runtime triple = %+v, want %+v", got, want)
				}
			})
		}
	}
}

// TestApplyLiveUpdateSwapsReviewerOnTurnConfig proves the plan actually lands
// on the session state Send reads. PlanLiveUpdate can be perfect and the
// switch still be inert if ApplyLiveUpdate forgets a field — and the reviewer
// is exactly the kind of field a struct-literal copy silently omits.
func TestApplyLiveUpdateSwapsReviewerOnTurnConfig(t *testing.T) {
	s := &Session{approvalsReviewer: approvalsReviewerUser}

	toAuto := codexLiveUpdateBaseOptions()
	toAuto.RuntimeMode = provider.RuntimeAuto
	update, ok := PlanLiveUpdate(codexLiveUpdateBaseOptions(), toAuto)
	if !ok {
		t.Fatal("PlanLiveUpdate into auto needs a restart")
	}
	s.ApplyLiveUpdate(update)
	if got := s.turnConfig().ApprovalsReviewer; got != approvalsReviewerAuto {
		t.Fatalf("turnConfig reviewer after switching into auto = %q, want %q", got, approvalsReviewerAuto)
	}

	back, ok := PlanLiveUpdate(toAuto, codexLiveUpdateBaseOptions())
	if !ok {
		t.Fatal("PlanLiveUpdate out of auto needs a restart")
	}
	s.ApplyLiveUpdate(back)
	if got := s.turnConfig().ApprovalsReviewer; got != approvalsReviewerUser {
		t.Fatalf("turnConfig reviewer after switching out of auto = %q, want %q", got, approvalsReviewerUser)
	}
}
