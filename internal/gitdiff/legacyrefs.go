package gitdiff

import (
	"context"
	"fmt"
	"strings"
)

// legacyCheckpointRefNamespace is the hidden ref namespace the removed
// per-message git-checkpoint machinery wrote snapshots under. Nothing
// writes here anymore; the sweeper below drains what old versions left
// behind.
const legacyCheckpointRefNamespace = "refs/agent-overflow/"

// CleanupLegacyCheckpointRefs deletes every ref under
// refs/agent-overflow/ in the workspace's repository and returns how
// many were deleted. Idempotent and cheap when the namespace is empty
// (one for-each-ref probe), so callers run it on every startup rather
// than tracking a done-flag — that also drains repos restored from
// backups. A non-repo workspace is a no-op, not an error.
func CleanupLegacyCheckpointRefs(ctx context.Context, workspace string) (int, error) {
	if !IsGitRepository(ctx, workspace) {
		return 0, nil
	}
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"for-each-ref", "--format=%(refname)", legacyCheckpointRefNamespace)
	if err != nil {
		return 0, fmt.Errorf("gitdiff: list legacy checkpoint refs: %w", err)
	}
	var refs []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.HasPrefix(line, legacyCheckpointRefNamespace) {
			refs = append(refs, line)
		}
	}
	if len(refs) == 0 {
		return 0, nil
	}
	var stdin strings.Builder
	for _, ref := range refs {
		stdin.WriteString("delete ")
		stdin.WriteString(ref)
		stdin.WriteByte('\n')
	}
	if _, _, _, err := runGitWithStdin(ctx, workspace, nil, []byte(stdin.String()), false, "update-ref", "--stdin"); err != nil {
		return 0, fmt.Errorf("gitdiff: delete %d legacy checkpoint refs: %w", len(refs), err)
	}
	return len(refs), nil
}
