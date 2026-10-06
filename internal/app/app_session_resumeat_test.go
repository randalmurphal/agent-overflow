package app

import (
	"testing"

	"agent-overflow/internal/provider/claude/sessionfork"
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

func writeResumeAtTestSession(t *testing.T) (projectsDir, workspace string) {
	t.Helper()
	home := t.TempDir()
	workspace = t.TempDir()
	writeClaudeProjectSession(t, home, workspace, "resume-at-session", resumeAtTestSession)
	return sessionfork.ProjectsDirForHome(home), workspace
}

// TestResolveClaudeResumeAtUsesBranchAwareScan pins the spawn-time cursor:
// the file-order content leaf a3-final is off the active branch, where a
// resume-at would kill the session pre-init, so the scan's deepest
// on-branch row decides.
func TestResolveClaudeResumeAtUsesBranchAwareScan(t *testing.T) {
	t.Parallel()
	projectsDir, workspace := writeResumeAtTestSession(t)
	if got := resolveClaudeResumeAt(projectsDir, "resume-at-session", workspace); got != "u2" {
		t.Fatalf("resolveClaudeResumeAt = %q, want u2 from the branch-aware scan", got)
	}
	// A missing session file resumes with no cursor: the app then omits
	// --resume-session-at entirely.
	if got := resolveClaudeResumeAt(projectsDir, "no-such-session", workspace); got != "" {
		t.Fatalf("resolveClaudeResumeAt = %q, want empty when the session file is missing", got)
	}
}
