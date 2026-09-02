package gitapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitroot"
)

// WorkspaceRef names a CHECKOUT — the subject of every workspace-scoped git
// RPC. A real thread carries its project id and workspace path; a draft
// placeholder that has no thread row yet carries the same two fields, so one
// wire shape serves both and no git RPC has to resolve a directory out of a
// conversation.
//
// WorkspacePath may be empty (the project root), the project root itself, or
// one of the project's registered worktrees. Anything else is refused by
// ResolveWorkspace.
type WorkspaceRef struct {
	ProjectID     string `json:"projectId"`
	WorkspacePath string `json:"workspacePath"`
}

// ResolveWorkspace validates ref and returns the (project root, canonical
// workspace) pair every workspace-scoped operation runs against.
//
// This is the trust boundary for caller-supplied workspace paths: the project
// id must name a project row, and the path must be that project's root or one
// of its worktrees. Every converted RPC goes through here, so no binding gets
// to invent its own notion of "close enough to the project".
//
// Membership is answered from git's on-disk layout by internal/gitroot, which
// NEVER spawns git — this runs on the hot path (every @-mention keystroke,
// every hunk-gap click, every status subscribe) and a `git worktree list` per
// call would be a regression on the DB read it replaced. gitroot.MainRoot also
// carries the two-way back-pointer confirmation a spoofed `.git` pointer has
// to beat, which a path-prefix test would not.
func (s *Service) ResolveWorkspace(ref WorkspaceRef) (project string, workspace string, err error) {
	project, err = s.ProjectPath(ref.ProjectID)
	if err != nil {
		return "", "", err
	}
	workspace = strings.TrimSpace(ref.WorkspacePath)
	if workspace == "" || gitops.SameFilesystemPath(workspace, project) {
		return project, project, nil
	}
	if !isCheckoutOf(workspace, project) {
		return "", "", fmt.Errorf("resolve workspace: %q is not a workspace of project %s", workspace, project)
	}
	return project, gitops.CanonicalPath(workspace), nil
}

// isCheckoutOf reports whether candidate is a CHECKOUT ROOT belonging to
// project. Both halves are load-bearing: MainRoot alone would also accept any
// SUBDIRECTORY of the project (it walks upward until it finds a `.git`), and a
// bare `.git` entry alone proves only that some repository lives here. Wanting
// a checkout root is what makes the accepted set exactly {project root, its
// worktrees} — a nested submodule owns its own root and resolves to itself.
func isCheckoutOf(candidate, project string) bool {
	if _, err := os.Lstat(filepath.Join(candidate, ".git")); err != nil {
		return false
	}
	root, ok := gitroot.MainRoot(candidate)
	return ok && gitops.SameFilesystemPath(root, project)
}
