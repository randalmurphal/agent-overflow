package threadmode

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestValidateCreate_EmptyDefaultsToChat(t *testing.T) {
	got, err := ValidateCreate("")
	if err != nil {
		t.Fatalf("ValidateCreate(\"\") error: %v", err)
	}
	if got != "chat" {
		t.Fatalf("ValidateCreate(\"\") = %q, want chat", got)
	}
}

func TestValidateCreate_TrimsWhitespace(t *testing.T) {
	got, err := ValidateCreate("  plan  ")
	if err != nil {
		t.Fatalf("ValidateCreate trimmed: %v", err)
	}
	if got != "plan" {
		t.Fatalf("got %q, want plan", got)
	}
}

func TestValidateCreate_RejectsDiscussion(t *testing.T) {
	if _, err := ValidateCreate("discussion"); err == nil {
		t.Fatal("expected ValidateCreate to reject discussion")
	}
}

func TestValidateCreate_RejectsUnknown(t *testing.T) {
	if _, err := ValidateCreate("bogus"); err == nil {
		t.Fatal("expected ValidateCreate to reject unknown mode")
	}
}

func TestValidateCreate_AcceptsLegal(t *testing.T) {
	for _, mode := range []string{"chat", "plan"} {
		if got, err := ValidateCreate(mode); err != nil || got != mode {
			t.Fatalf("ValidateCreate(%q) = (%q, %v); want (%q, nil)", mode, got, err, mode)
		}
	}
}

func TestValidateCreate_RejectsRetiredDesign(t *testing.T) {
	if _, err := ValidateCreate("design"); err == nil {
		t.Fatal("expected ValidateCreate to reject retired design mode")
	}
}

func TestValidateSet_RejectsDiscussion(t *testing.T) {
	if _, err := ValidateSet("discussion"); err == nil {
		t.Fatal("expected ValidateSet to reject discussion (immutable thread type)")
	}
}

func TestValidateSet_RejectsEmpty(t *testing.T) {
	// Unlike ValidateCreate, set rejects empty — UpdateThreadMode is
	// only called from explicit user actions.
	if _, err := ValidateSet(""); err == nil {
		t.Fatal("expected ValidateSet to reject empty mode")
	}
}

func TestValidateSet_AcceptsChatAndPlan(t *testing.T) {
	for _, mode := range []string{"chat", "plan"} {
		if got, err := ValidateSet(mode); err != nil || got != mode {
			t.Fatalf("ValidateSet(%q) = (%q, %v); want (%q, nil)", mode, got, err, mode)
		}
	}
}

func TestIsPostCreationMode(t *testing.T) {
	cases := map[string]bool{
		"chat":       true,
		"plan":       true,
		"  plan  ":   true,
		"design":     false,
		"discussion": false,
		"":           false,
		"bogus":      false,
	}
	for in, want := range cases {
		if got := IsPostCreationMode(in); got != want {
			t.Errorf("IsPostCreationMode(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestParseRuntime_AllLegal iterates provider.AllRuntimeModes rather than a
// local list so a mode the provider package considers canonical can never be
// rejected here — the parser is the wire-facing gate for the same value set.
func TestParseRuntime_AllLegal(t *testing.T) {
	if len(provider.AllRuntimeModes) == 0 {
		t.Fatal("provider.AllRuntimeModes is empty")
	}
	for _, m := range provider.AllRuntimeModes {
		got, err := ParseRuntime(string(m))
		if err != nil {
			t.Fatalf("ParseRuntime(%q): %v", m, err)
		}
		if got != m {
			t.Fatalf("ParseRuntime(%q) = %q, want %q", m, got, m)
		}
	}
}

func TestParseRuntime_RejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "anything", "  ", "Approval-Required", "Read-Only", "readonly"} {
		if _, err := ParseRuntime(in); err == nil {
			t.Errorf("ParseRuntime(%q) should fail", in)
		}
	}
}

func TestParseOptionalRuntime_EmptyIsAbsent(t *testing.T) {
	got, present, err := ParseOptionalRuntime("")
	if err != nil {
		t.Fatalf("ParseOptionalRuntime(\"\"): %v", err)
	}
	if present {
		t.Fatalf("present = true for empty input")
	}
	if got != "" {
		t.Fatalf("got = %q, want empty", got)
	}
}

func TestParseOptionalRuntime_PresentValid(t *testing.T) {
	got, present, err := ParseOptionalRuntime(string(provider.RuntimeAutoAcceptEdits))
	if err != nil {
		t.Fatalf("ParseOptionalRuntime: %v", err)
	}
	if !present {
		t.Fatal("present = false on a non-empty value")
	}
	if got != provider.RuntimeAutoAcceptEdits {
		t.Fatalf("got = %q, want %q", got, provider.RuntimeAutoAcceptEdits)
	}
}

func TestParseOptionalRuntime_PresentInvalid(t *testing.T) {
	_, present, err := ParseOptionalRuntime("bogus")
	if err == nil {
		t.Fatal("expected error for bogus runtime mode")
	}
	if present {
		t.Fatal("present should be false on error")
	}
}

func TestWorkflowModesAreLegalSagaOwnedAndHidden(t *testing.T) {
	for _, mode := range []string{ModeWorkflow, ModeWorkflowStudio, ModeWorkflowTriage} {
		if !IsLegal(mode) || !IsSagaOwned(mode) || !IsHidden(mode) {
			t.Fatalf("mode %q legal/saga-owned/hidden = %v/%v/%v", mode, IsLegal(mode), IsSagaOwned(mode), IsHidden(mode))
		}
		if _, err := ValidateCreate(mode); err == nil {
			t.Fatalf("ValidateCreate(%q) succeeded", mode)
		}
		if _, err := ValidateSet(mode); err == nil {
			t.Fatalf("ValidateSet(%q) succeeded", mode)
		}
	}
	if IsHidden(ModeDiscussion) || !IsSagaOwned(ModeDiscussion) {
		t.Fatalf("discussion hidden/saga-owned = %v/%v", IsHidden(ModeDiscussion), IsSagaOwned(ModeDiscussion))
	}
	if IsHidden(ModeTerminal) || !IsLegal(ModeTerminal) {
		t.Fatalf("terminal hidden/legal = %v/%v", IsHidden(ModeTerminal), IsLegal(ModeTerminal))
	}
	got := HiddenModes()
	got[0] = "mutated"
	if !IsHidden(ModeWorkflow) {
		t.Fatal("HiddenModes exposed mutable package state")
	}
}

// TestParseOptionalRuntime_AcceptsReadOnly covers the restricted tier through
// the optional path, which is what the settings / new-thread-defaults callers
// use. Rejecting it there would make the mode unselectable everywhere except
// the workflow runner.
func TestParseOptionalRuntime_AcceptsReadOnly(t *testing.T) {
	got, present, err := ParseOptionalRuntime(string(provider.RuntimeReadOnly))
	if err != nil {
		t.Fatalf("ParseOptionalRuntime(read-only): %v", err)
	}
	if !present {
		t.Fatal("present = false for read-only")
	}
	if got != provider.RuntimeReadOnly {
		t.Fatalf("got = %q, want read-only", got)
	}
}
