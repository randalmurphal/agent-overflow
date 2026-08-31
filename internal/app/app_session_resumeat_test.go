package app

import (
	"testing"
)

// resumeAtTestSession is the incident topology (see
// docs/references/fixtures/claude/session_api_error_offbranch.jsonl):
// deferred system/api_error rows landed at the file tail with a stale
// parentUuid, so the active branch from the last transcript row (err2)
// is err2 → err1 → u2 → a1 → u1 and the file-order content leaf
// a3-final is OFF it. Resuming at a3-final hard-fails in the real CLI.
const resumeAtTestSession = `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"please run the checks"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"msg_01","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"checks passed"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","message":{"id":"msg_02","role":"assistant","content":[{"type":"text","text":"working"}]}}
{"type":"assistant","uuid":"a3-final","parentUuid":"a2","message":{"id":"msg_03","role":"assistant","content":[{"type":"text","text":"All green"}]}}
{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"u2","level":"error","retryAttempt":1,"error":{"message":"Connection error."}}
{"type":"system","subtype":"api_error","uuid":"err2","parentUuid":"err1","level":"error","retryAttempt":2,"error":{"message":"Connection error."}}
`

func writeResumeAtTestSession(t *testing.T) (workspace string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace = t.TempDir()
	writeClaudeProjectSession(t, home, workspace, "resume-at-session", resumeAtTestSession)
	return workspace
}

// TestResolveClaudeResumeAtRejectsOffBranchExplicit pins the spawn-time
// guard: an explicit cursor off the active branch (the wire-derived
// live-tracker leaf after the CLI rewrote the branch under it) must be
// rejected and replaced by the branch-aware scan's pick, never passed
// through to --resume-session-at where it would kill the session
// pre-init.
func TestResolveClaudeResumeAtRejectsOffBranchExplicit(t *testing.T) {
	workspace := writeResumeAtTestSession(t)
	got := resolveClaudeResumeAt(testProviderProjectsDir(t), "resume-at-session", workspace, "a3-final")
	if got == "a3-final" {
		t.Fatalf("resolveClaudeResumeAt passed off-branch explicit cursor a3-final through")
	}
	if got != "u2" {
		t.Fatalf("resolveClaudeResumeAt = %q, want scan fallback u2 (deepest on-branch content row)", got)
	}
}

func TestResolveClaudeResumeAtAcceptsOnBranchExplicit(t *testing.T) {
	workspace := writeResumeAtTestSession(t)
	// u1 is on-branch but NOT the scan's pick (u2 is deeper) — proves the
	// explicit cursor was honored rather than recomputed.
	if got := resolveClaudeResumeAt(testProviderProjectsDir(t), "resume-at-session", workspace, "u1"); got != "u1" {
		t.Fatalf("resolveClaudeResumeAt = %q, want explicit on-branch cursor u1 honored", got)
	}
}

func TestResolveClaudeResumeAtFallsBackToScan(t *testing.T) {
	workspace := writeResumeAtTestSession(t)
	// No explicit cursor → branch-aware scan decides.
	if got := resolveClaudeResumeAt(testProviderProjectsDir(t), "resume-at-session", workspace, ""); got != "u2" {
		t.Fatalf("resolveClaudeResumeAt = %q, want u2 from the branch-aware scan", got)
	}
	// Unverifiable explicit cursor (session file missing) → empty, the
	// app then omits --resume-session-at entirely.
	if got := resolveClaudeResumeAt(testProviderProjectsDir(t), "no-such-session", workspace, "u1"); got != "" {
		t.Fatalf("resolveClaudeResumeAt = %q, want empty when the session file is missing", got)
	}
}
