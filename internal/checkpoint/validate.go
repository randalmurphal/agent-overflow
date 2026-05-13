package checkpoint

import (
	"fmt"
	"strings"
)

// ValidateRef rejects a checkpoint ref that isn't well-formed for the
// owning thread. It guards against three failure modes the
// checkpoint-row callers all care about:
//
//  1. Empty ref name — a row without a hidden ref cannot be diffed
//     or restored.
//  2. Ref outside this thread's namespace — the checkpoint sub-tree
//     is per-thread, so a ref that doesn't sit under
//     `refs/agent-overflow/checkpoints/<encoded-thread>/...` would be
//     a sign of a bug or a cross-thread leak.
//  3. Empty workspace path — diff/restore commands need a workdir.
//
// `action` is the operation tag the caller wants surfaced in the
// resulting error (e.g. "fork thread", "revert checkpoint"). It
// becomes the `%s:` prefix on each returned error.
func ValidateRef(action, threadID, refName, workspacePath string) error {
	if strings.TrimSpace(refName) == "" {
		return fmt.Errorf("%s: checkpoint ref is empty", action)
	}
	if !IsThreadRef(refName, threadID) {
		return fmt.Errorf("%s: checkpoint ref %q is outside thread %q namespace", action, refName, threadID)
	}
	if strings.TrimSpace(workspacePath) == "" {
		return fmt.Errorf("%s: checkpoint workspace is empty for ref %q", action, refName)
	}
	return nil
}

// ValidateWorkspaceMatch reports the resolved workspace if the
// thread's workspace path matches the checkpoint row's recorded
// workspace path, or an error otherwise. Used by revert / preview
// flows before shelling out to git: an automatic checkpoint captured
// against workspace X cannot meaningfully restore into workspace Y.
//
// `threadWorkspace` is the live workspace path; `checkpointWorkspace`
// is the path recorded on the captured row.
func ValidateWorkspaceMatch(action, threadWorkspace, checkpointWorkspace string) (string, error) {
	if threadWorkspace == "" || threadWorkspace != checkpointWorkspace {
		return "", fmt.Errorf("%s: checkpoint workspace %q does not match thread workspace %q", action, checkpointWorkspace, threadWorkspace)
	}
	return threadWorkspace, nil
}
