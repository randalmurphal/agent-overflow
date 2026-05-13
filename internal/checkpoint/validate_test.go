package checkpoint

import (
	"strings"
	"testing"
)

func TestValidateRefRejectsEmptyRef(t *testing.T) {
	err := ValidateRef("fork", "thread-1", "", "/workspace")
	if err == nil {
		t.Fatal("ValidateRef(empty ref) = nil, want error")
	}
	if !strings.Contains(err.Error(), "fork: checkpoint ref is empty") {
		t.Fatalf("error = %v, want action prefix + empty ref message", err)
	}
}

func TestValidateRefRejectsWhitespaceRef(t *testing.T) {
	if err := ValidateRef("fork", "thread-1", "   ", "/workspace"); err == nil {
		t.Fatal("ValidateRef(whitespace ref) = nil, want error")
	}
}

func TestValidateRefRejectsOutsideNamespace(t *testing.T) {
	err := ValidateRef("revert", "thread-1", "refs/heads/main", "/workspace")
	if err == nil {
		t.Fatal("ValidateRef(non-namespace) = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside thread") {
		t.Fatalf("error = %v, want namespace-violation message", err)
	}
}

func TestValidateRefRejectsCrossThreadRef(t *testing.T) {
	other := RefForThreadTurn("thread-2", 5)
	if err := ValidateRef("revert", "thread-1", other, "/workspace"); err == nil {
		t.Fatal("ValidateRef(cross-thread ref) = nil, want error")
	}
}

func TestValidateRefRejectsEmptyWorkspace(t *testing.T) {
	good := RefForThreadTurn("thread-1", 5)
	err := ValidateRef("revert", "thread-1", good, "")
	if err == nil {
		t.Fatal("ValidateRef(empty workspace) = nil, want error")
	}
	if !strings.Contains(err.Error(), "workspace is empty") {
		t.Fatalf("error = %v, want empty-workspace message", err)
	}
}

func TestValidateRefAcceptsWellFormed(t *testing.T) {
	good := RefForThreadTurn("thread-1", 5)
	if err := ValidateRef("revert", "thread-1", good, "/workspace"); err != nil {
		t.Fatalf("ValidateRef(well-formed) err = %v", err)
	}
}

func TestValidateWorkspaceMatchAccepts(t *testing.T) {
	got, err := ValidateWorkspaceMatch("revert", "/workspace", "/workspace")
	if err != nil {
		t.Fatalf("ValidateWorkspaceMatch(equal) err = %v", err)
	}
	if got != "/workspace" {
		t.Fatalf("ValidateWorkspaceMatch returned %q, want %q", got, "/workspace")
	}
}

func TestValidateWorkspaceMatchRejectsEmptyThreadWorkspace(t *testing.T) {
	if _, err := ValidateWorkspaceMatch("revert", "", "/workspace"); err == nil {
		t.Fatal("ValidateWorkspaceMatch(empty thread ws) = nil, want error")
	}
}

func TestValidateWorkspaceMatchRejectsMismatch(t *testing.T) {
	_, err := ValidateWorkspaceMatch("revert", "/thread-ws", "/checkpoint-ws")
	if err == nil {
		t.Fatal("ValidateWorkspaceMatch(mismatch) = nil, want error")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want mismatch message", err)
	}
}
