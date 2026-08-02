package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// liveSettingsNotification is the exact `thread/settings/updated` payload
// captured from codex-cli 0.146.0 (scratchpad spike-codex,
// captures2/notifications-exp.json), trimmed only of the transport's
// emittedAtMs envelope field. Using the real capture rather than a
// hand-written fixture is what makes this a wire test.
const liveSettingsNotification = `{
  "threadId": "019fc2ff-9050-7971-ac4e-b902cc3b9f00",
  "threadSettings": {
    "cwd": "/tmp/work",
    "approvalPolicy": "on-request",
    "approvalsReviewer": "auto_review",
    "sandboxPolicy": {
      "type": "workspaceWrite",
      "writableRoots": [],
      "networkAccess": false,
      "excludeTmpdirEnvVar": false,
      "excludeSlashTmp": false
    },
    "activePermissionProfile": null,
    "model": "gpt-5.6-sol",
    "modelProvider": "openai",
    "serviceTier": "priority",
    "effort": "high",
    "summary": null,
    "collaborationMode": {"mode": "default", "settings": {"model": "gpt-5.6-sol"}},
    "multiAgentMode": "explicitRequestOnly",
    "personality": "pragmatic"
  }
}`

func settingsSession(codexThreadID string) *Session {
	s := &Session{threadID: testThread}
	s.setRootThreadID(codexThreadID)
	return s
}

// TestReconcileThreadSettings_DecodesLiveCapture pins every field we
// project out of the real 0.146.0 notification, including the
// camelCase-to-AO sandbox translation.
func TestReconcileThreadSettings_DecodesLiveCapture(t *testing.T) {
	s := settingsSession("019fc2ff-9050-7971-ac4e-b902cc3b9f00")
	s.reconcileThreadSettings(json.RawMessage(liveSettingsNotification))

	got, known := s.ObservedThreadSettings()
	if !known {
		t.Fatal("ObservedThreadSettings reported nothing after a valid notification")
	}
	want := ThreadSettings{
		Cwd:               "/tmp/work",
		Model:             "gpt-5.6-sol",
		ModelProvider:     "openai",
		ReasoningEffort:   "high",
		ServiceTier:       "priority",
		ApprovalPolicy:    "on-request",
		ApprovalsReviewer: "auto_review",
		Sandbox:           "workspace-write",
	}
	if got != want {
		t.Errorf("observed settings\n got: %+v\nwant: %+v", got, want)
	}
}

// TestReconcileThreadSettings_SandboxTranslation covers every sandbox tag
// Codex can send plus an unmodelled one. Guessing a tier for an unknown
// tag would report a sandbox that is not the one being enforced, so the
// contract is to report none.
func TestReconcileThreadSettings_SandboxTranslation(t *testing.T) {
	cases := []struct {
		wire string
		want string
	}{
		{wire: "readOnly", want: "read-only"},
		{wire: "workspaceWrite", want: "workspace-write"},
		{wire: "dangerFullAccess", want: "danger-full-access"},
		{wire: "someFutureTier", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			s := settingsSession("th")
			s.reconcileThreadSettings(json.RawMessage(
				`{"threadId":"th","threadSettings":{"sandboxPolicy":{"type":"` + tc.wire + `"}}}`))
			got, _ := s.ObservedThreadSettings()
			if got.Sandbox != tc.want {
				t.Errorf("sandbox for %q = %q, want %q", tc.wire, got.Sandbox, tc.want)
			}
		})
	}
}

// TestReconcileThreadSettings_NullOverridesReadAsEmpty pins the nullable
// fields. Upstream sends null for "no override in force"; decoding that as
// a literal value would make the observed snapshot claim a tier or effort
// the thread does not have.
func TestReconcileThreadSettings_NullOverridesReadAsEmpty(t *testing.T) {
	s := settingsSession("th")
	s.reconcileThreadSettings(json.RawMessage(
		`{"threadId":"th","threadSettings":{"model":"gpt","effort":null,"serviceTier":null}}`))
	got, known := s.ObservedThreadSettings()
	if !known {
		t.Fatal("expected the notification to be recorded")
	}
	if got.ReasoningEffort != "" || got.ServiceTier != "" {
		t.Errorf("null effort/serviceTier must read as empty, got %+v", got)
	}
}

// TestReconcileThreadSettings_IgnoresForeignThread keeps a spawned collab
// child's configuration out of the root's snapshot. A child runs its own
// model, and letting it land here would misattribute the parent's tokens
// to the child's model.
func TestReconcileThreadSettings_IgnoresForeignThread(t *testing.T) {
	s := settingsSession("root-thread")
	s.reconcileThreadSettings(json.RawMessage(
		`{"threadId":"child-thread","threadSettings":{"model":"child-model"}}`))
	if got, known := s.ObservedThreadSettings(); known {
		t.Errorf("a child thread's settings were recorded as the root's: %+v", got)
	}
}

// TestReconcileThreadSettings_IgnoresUndecodableParams keeps a malformed
// frame from wiping a good snapshot.
func TestReconcileThreadSettings_IgnoresUndecodableParams(t *testing.T) {
	s := settingsSession("th")
	s.reconcileThreadSettings(json.RawMessage(`{"threadId":"th","threadSettings":{"model":"gpt"}}`))
	s.reconcileThreadSettings(json.RawMessage(`not json`))
	got, known := s.ObservedThreadSettings()
	if !known || got.Model != "gpt" {
		t.Errorf("malformed frame clobbered the snapshot: known=%v got=%+v", known, got)
	}
}

// TestCurrentModelPrefersObservedOverRequested walks the whole transition
// sequence usage attribution depends on, in order:
//
//	requested-only → Codex reports A → user selects B (pending) → Codex reports B
//
// The third step is the one that matters: ApplyLiveUpdate(B) only changes
// what the NEXT turn will ask for, so tokens billed by the turn that ran
// on A must still be attributed to A. Reading the requested field here —
// the behavior before this change — put them on B.
func TestCurrentModelPrefersObservedOverRequested(t *testing.T) {
	s := settingsSession("th")
	s.ApplyLiveUpdate(LiveUpdate{Model: "model-a"})
	if got := s.currentModel(); got != "model-a" {
		t.Fatalf("with nothing observed, currentModel = %q, want the requested model-a", got)
	}

	s.reconcileThreadSettings(json.RawMessage(`{"threadId":"th","threadSettings":{"model":"model-a"}}`))
	if got := s.currentModel(); got != "model-a" {
		t.Fatalf("after Codex confirmed model-a, currentModel = %q", got)
	}

	s.ApplyLiveUpdate(LiveUpdate{Model: "model-b"})
	if got := s.currentModel(); got != "model-a" {
		t.Errorf("a pending selection changed attribution before the turn ran: currentModel = %q, want model-a", got)
	}

	s.reconcileThreadSettings(json.RawMessage(`{"threadId":"th","threadSettings":{"model":"model-b"}}`))
	if got := s.currentModel(); got != "model-b" {
		t.Errorf("after Codex confirmed model-b, currentModel = %q", got)
	}
}

// TestCurrentModelFallsBackWhenCodexReportsNoModel guards the fallback:
// a settings frame that carries no model must not blank out attribution.
func TestCurrentModelFallsBackWhenCodexReportsNoModel(t *testing.T) {
	s := settingsSession("th")
	s.ApplyLiveUpdate(LiveUpdate{Model: "model-a"})
	s.reconcileThreadSettings(json.RawMessage(`{"threadId":"th","threadSettings":{"approvalPolicy":"never"}}`))
	if got := s.currentModel(); got != "model-a" {
		t.Errorf("currentModel = %q, want the requested model-a as fallback", got)
	}
}

// TestReconcileThreadSettings_LaterFrameReplacesEarlier pins that the
// snapshot is a replacement, not a merge: a field Codex stops sending has
// stopped being in force, and keeping the stale value would report an
// override that no longer exists.
func TestReconcileThreadSettings_LaterFrameReplacesEarlier(t *testing.T) {
	s := settingsSession("th")
	s.reconcileThreadSettings(json.RawMessage(
		`{"threadId":"th","threadSettings":{"model":"gpt","serviceTier":"priority","effort":"high"}}`))
	s.reconcileThreadSettings(json.RawMessage(
		`{"threadId":"th","threadSettings":{"model":"gpt","serviceTier":null,"effort":null}}`))
	got, _ := s.ObservedThreadSettings()
	if got.ServiceTier != "" || got.ReasoningEffort != "" {
		t.Errorf("cleared overrides survived the replacement: %+v", got)
	}
}

// TestDispatchNotificationReconcilesThreadSettings proves the wiring: a
// settings notification arriving on the real dispatch path updates the
// snapshot and produces no transcript event.
func TestDispatchNotificationReconcilesThreadSettings(t *testing.T) {
	var events []string
	s := &Session{
		threadID: testThread,
		onEvent:  func(e provider.ProviderEvent) { events = append(events, string(e.Kind)) },
	}
	s.setRootThreadID("019fc2ff-9050-7971-ac4e-b902cc3b9f00")
	output := captureLog(t, func() {
		s.dispatchNotification("thread/settings/updated", json.RawMessage(liveSettingsNotification))
	})
	if output != "" {
		t.Errorf("a handled notification must not reach the drift log, got:\n%s", output)
	}
	got, known := s.ObservedThreadSettings()
	if !known || got.Model != "gpt-5.6-sol" {
		t.Errorf("dispatch did not reconcile settings: known=%v got=%+v", known, got)
	}
	if len(events) != 0 {
		t.Errorf("settings reconciliation must not emit transcript events, got %v", events)
	}
}
