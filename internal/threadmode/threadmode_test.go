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
	for _, mode := range []string{"chat", "plan", "design"} {
		if got, err := ValidateCreate(mode); err != nil || got != mode {
			t.Fatalf("ValidateCreate(%q) = (%q, %v); want (%q, nil)", mode, got, err, mode)
		}
	}
}

func TestValidateSet_RejectsDesign(t *testing.T) {
	if _, err := ValidateSet("design"); err == nil {
		t.Fatal("expected ValidateSet to reject design (immutable thread type)")
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

func TestParseRuntime_AllLegal(t *testing.T) {
	legal := []provider.RuntimeMode{
		provider.RuntimeApprovalRequired,
		provider.RuntimeAutoAcceptEdits,
		provider.RuntimeFullAccess,
	}
	for _, m := range legal {
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
	for _, in := range []string{"", "anything", "  ", "Approval-Required"} {
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
