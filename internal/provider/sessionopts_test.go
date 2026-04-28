package provider

import "testing"

// fakeThreadView is a minimal ThreadView stub used to exercise the
// translation logic without dragging in internal/store/.
type fakeThreadView struct {
	provider                   string
	model                      string
	workspacePath              string
	reasoningEffort            string
	fastMode                   bool
	contextWindow              int
	autoCompactStandardPercent int
	autoCompactExtendedPercent int
	mode                       string
	runtimeMode                string
	sessionRef                 string
	pendingForkRef             string
}

func (f fakeThreadView) GetProvider() string        { return f.provider }
func (f fakeThreadView) GetModel() string           { return f.model }
func (f fakeThreadView) GetWorkspacePath() string   { return f.workspacePath }
func (f fakeThreadView) GetReasoningEffort() string { return f.reasoningEffort }
func (f fakeThreadView) GetFastMode() bool          { return f.fastMode }
func (f fakeThreadView) GetContextWindow() int      { return f.contextWindow }
func (f fakeThreadView) GetAutoCompactStandardPercent() int {
	return f.autoCompactStandardPercent
}
func (f fakeThreadView) GetAutoCompactExtendedPercent() int {
	return f.autoCompactExtendedPercent
}
func (f fakeThreadView) GetMode() string           { return f.mode }
func (f fakeThreadView) GetRuntimeMode() string    { return f.runtimeMode }
func (f fakeThreadView) GetSessionRef() string     { return f.sessionRef }
func (f fakeThreadView) GetPendingForkRef() string { return f.pendingForkRef }

func TestSessionOptionsFromThreadCopiesEveryField(t *testing.T) {
	view := fakeThreadView{
		provider:                   "claude",
		model:                      "claude-sonnet-4-6",
		workspacePath:              "/tmp/workspace",
		reasoningEffort:            "xhigh",
		fastMode:                   true,
		contextWindow:              1000000,
		autoCompactExtendedPercent: 80,
		mode:                       "plan",
		runtimeMode:                "auto-accept-edits",
		sessionRef:                 "session-abc",
	}

	opts := SessionOptionsFromThread(view, AutoCompactDefaults{}, "system prompt", false)

	if opts.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", opts.Provider)
	}
	if opts.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", opts.Model)
	}
	if opts.WorkDir != "/tmp/workspace" {
		t.Errorf("WorkDir = %q, want /tmp/workspace", opts.WorkDir)
	}
	if opts.ReasoningEffort != EffortXHigh {
		t.Errorf("ReasoningEffort = %q, want xhigh", opts.ReasoningEffort)
	}
	if !opts.FastMode {
		t.Error("FastMode = false, want true")
	}
	if opts.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000", opts.ContextWindow)
	}
	if opts.AutoCompactPercent != 80 {
		t.Errorf("AutoCompactPercent = %d, want 80", opts.AutoCompactPercent)
	}
	if opts.Mode != ModePlan {
		t.Errorf("Mode = %q, want plan", opts.Mode)
	}
	if opts.RuntimeMode != RuntimeAutoAcceptEdits {
		t.Errorf("RuntimeMode = %q, want auto-accept-edits", opts.RuntimeMode)
	}
	if opts.Resume != "session-abc" {
		t.Errorf("Resume = %q, want session-abc", opts.Resume)
	}
	if opts.ForkSession {
		t.Error("ForkSession = true, want false")
	}
	if opts.SystemPrompt != "system prompt" {
		t.Errorf("SystemPrompt = %q, want %q", opts.SystemPrompt, "system prompt")
	}
}

func TestSessionOptionsFromThreadForkPrefersPendingRef(t *testing.T) {
	view := fakeThreadView{
		provider:       "claude",
		sessionRef:     "live-session",
		pendingForkRef: "pending-fork",
	}

	opts := SessionOptionsFromThread(view, AutoCompactDefaults{}, "", true)
	if opts.Resume != "pending-fork" {
		t.Errorf("Resume = %q, want pending-fork (fork should consume PendingForkRef)", opts.Resume)
	}
	if !opts.ForkSession {
		t.Error("ForkSession = false, want true")
	}
}

func TestSessionOptionsFromThreadForkFallsBackToSessionRef(t *testing.T) {
	view := fakeThreadView{
		provider:   "claude",
		sessionRef: "live-session",
		// PendingForkRef intentionally empty.
	}

	opts := SessionOptionsFromThread(view, AutoCompactDefaults{}, "", true)
	if opts.Resume != "live-session" {
		t.Errorf("Resume = %q, want live-session (no pending fork ref)", opts.Resume)
	}
}

func TestSessionOptionsFromThreadNormalizesInvalidEnums(t *testing.T) {
	view := fakeThreadView{
		provider:        "codex",
		reasoningEffort: "nope",
		mode:            "bogus",
		runtimeMode:     "unknown",
	}

	opts := SessionOptionsFromThread(view, AutoCompactDefaults{}, "", false)
	if opts.ReasoningEffort != DefaultReasoningEffort {
		t.Errorf("ReasoningEffort = %q, want default %q", opts.ReasoningEffort, DefaultReasoningEffort)
	}
	if opts.Mode != DefaultInteractionMode {
		t.Errorf("Mode = %q, want default %q", opts.Mode, DefaultInteractionMode)
	}
	if opts.RuntimeMode != DefaultRuntimeMode {
		t.Errorf("RuntimeMode = %q, want default %q", opts.RuntimeMode, DefaultRuntimeMode)
	}
}

func TestNormalizeReasoningEffortKnownValues(t *testing.T) {
	for _, eff := range AllReasoningEfforts {
		if got := NormalizeReasoningEffort(string(eff)); got != eff {
			t.Errorf("NormalizeReasoningEffort(%q) = %q, want %q", string(eff), got, eff)
		}
	}
}

func TestNormalizeInteractionModeKnownValues(t *testing.T) {
	for _, m := range AllInteractionModes {
		if got := NormalizeInteractionMode(string(m)); got != m {
			t.Errorf("NormalizeInteractionMode(%q) = %q, want %q", string(m), got, m)
		}
	}
}
