package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/commitmsg"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// The commit dialog's reads: the files there are to commit, one file's
// patch, and the text a generated message describes. All three read a
// worktree snapshot, so none writes the workspace's index.

// CommitFile is one row of the commit dialog's file list.
type CommitFile = gitdiff.ChangedFile

// CommitSelection names what GitCommit commits and GenerateCommitMessage
// describes: every uncommitted change, or exactly the given files. One of
// the two is set, so a caller that lost its selection gets an error, not a
// commit of everything.
type CommitSelection struct {
	// All covers every uncommitted change, untracked files included.
	All bool `json:"all"`
	// Files are rows as ListCommitFiles returned them.
	Files []CommitFile `json:"files"`
}

func (s CommitSelection) validate() error {
	if s.All {
		if len(s.Files) > 0 {
			return errors.New("commit selection: files given with all")
		}
		return nil
	}
	if len(s.Files) == 0 {
		return errors.New("commit selection: no files selected")
	}
	for _, file := range s.Files {
		if file.Path == "" {
			return errors.New("commit selection: a file without a path")
		}
	}
	return nil
}

// ErrCommitFilesChanged is returned by GitCommit when a selected file is no
// longer the change ListCommitFiles listed.
var ErrCommitFilesChanged = errors.New("the selected files changed since they were listed")

// maxNamedChangedFiles bounds the paths an ErrCommitFilesChanged names.
const maxNamedChangedFiles = 3

// listedCommitSelection turns a selection into the paths git commits, after
// checking that each selected file is still the change the caller listed:
// the same paths, status and working-tree content. A file edited since (an
// agent is free to write while the dialog is open) fails the commit, so
// nothing is recorded that the dialog did not show.
func (a *App) listedCommitSelection(ctx context.Context, workspace string, selection CommitSelection) (gitops.CommitSelection, error) {
	if err := selection.validate(); err != nil {
		return gitops.CommitSelection{}, err
	}
	if selection.All {
		return gitops.CommitSelection{All: true}, nil
	}
	tempRoot, err := a.reviewDiffSnapshotRoot()
	if err != nil {
		return gitops.CommitSelection{}, err
	}
	files, err := gitdiff.WorktreeChanges(ctx, workspace, tempRoot)
	if err != nil {
		return gitops.CommitSelection{}, err
	}
	current := make(map[string]CommitFile, len(files))
	for _, file := range files {
		current[file.Path] = file
	}
	paths := make([]string, 0, len(selection.Files))
	var changed []string
	for _, file := range selection.Files {
		now, listed := current[file.Path]
		if !listed || now.OldPath != file.OldPath || now.Status != file.Status || now.Version != file.Version {
			changed = append(changed, file.Path)
			continue
		}
		if file.OldPath != "" {
			paths = append(paths, file.OldPath)
		}
		paths = append(paths, file.Path)
	}
	if len(changed) > 0 {
		named := strings.Join(changed[:min(len(changed), maxNamedChangedFiles)], ", ")
		if more := len(changed) - maxNamedChangedFiles; more > 0 {
			named = fmt.Sprintf("%s and %d more", named, more)
		}
		return gitops.CommitSelection{}, fmt.Errorf("%w (%s); review them and commit again", ErrCommitFilesChanged, named)
	}
	return gitops.CommitSelection{Paths: paths}, nil
}

// maxCommitFiles bounds the file list one ListCommitFiles answer carries.
const maxCommitFiles = 5000

// CommitFiles answers ListCommitFiles.
type CommitFiles struct {
	// Files is every uncommitted change by path, up to maxCommitFiles.
	Files []CommitFile `json:"files"`
	// Total counts the changes before the bound.
	Total int `json:"total"`
	// WholeOnly reports that git records only a commit of every change
	// right now (a merge, cherry-pick or revert is in progress), so the
	// selection GitCommit takes is CommitSelection.All.
	WholeOnly bool `json:"wholeOnly"`
}

// ListCommitFiles lists what the referenced workspace has to commit:
// tracked changes against HEAD plus untracked-not-ignored files, a rename
// as one entry. Empty for non-git workspaces.
//
//ao:scope git:operate
func (a *App) ListCommitFiles(ctx context.Context, ws WorkspaceRef) (CommitFiles, error) {
	const action = "list commit files"
	if a.shuttingDown.Load() {
		return CommitFiles{}, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return CommitFiles{}, err
	}
	if !gitdiff.IsGitRepository(ctx, workspace) {
		return CommitFiles{Files: []CommitFile{}}, nil
	}
	tempRoot, err := a.reviewDiffSnapshotRoot()
	if err != nil {
		return CommitFiles{}, fmt.Errorf("%s: %w", action, err)
	}
	files, err := gitdiff.WorktreeChanges(ctx, workspace, tempRoot)
	if err != nil {
		return CommitFiles{}, fmt.Errorf("%s: %w", action, err)
	}
	out := CommitFiles{Files: files, Total: len(files), WholeOnly: a.gitCore().CommitsWholeOnly(workspace)}
	if out.Files == nil {
		out.Files = []CommitFile{}
	}
	if len(out.Files) > maxCommitFiles {
		out.Files = out.Files[:maxCommitFiles]
	}
	return out, nil
}

// OpenCommitFileDiff opens the uncommitted patch of the given paths of the
// referenced workspace, each matched literally: one row of ListCommitFiles,
// with both paths of a rename.
//
//ao:scope files:read
func (a *App) OpenCommitFileDiff(ctx context.Context, ws WorkspaceRef, paths []string) (ReviewDiffOpened, error) {
	const action = "open commit file diff"
	if a.shuttingDown.Load() {
		return ReviewDiffOpened{}, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return ReviewDiffOpened{}, err
	}
	if !gitdiff.IsGitRepository(ctx, workspace) {
		return ReviewDiffOpened{}, fmt.Errorf("%s: workspace is not a git repository", action)
	}
	return a.openReviewDiff(ctx, action, transport.ScopeFilesRead, func(tempRoot string) (*gitdiff.Diff, error) {
		return gitdiff.OpenWorktreeFilesDiff(ctx, workspace, tempRoot, paths, gitdiff.Options{})
	})
}

// commitPatchPathBytes bounds the path bytes of a selection handed to the
// patch read as arguments. The prompt keeps commitmsg.PromptStagedPatchLimit
// of the patch, so files past this bound would not have reached it; the
// summary still names them.
const commitPatchPathBytes = 32 << 10

// commitContext returns what a commit of the selection would contain, read
// from one snapshot: a name-status summary and the start of its patch. The
// summary is empty when the selection has nothing uncommitted.
func (a *App) commitContext(ctx context.Context, workspace string, selection CommitSelection) (summary, patch string, err error) {
	if err := selection.validate(); err != nil {
		return "", "", err
	}
	tempRoot, err := a.reviewDiffSnapshotRoot()
	if err != nil {
		return "", "", err
	}
	snapshot, err := gitdiff.CaptureWorktree(ctx, workspace, tempRoot)
	if err != nil {
		return "", "", err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	files, err := snapshot.Changes(ctx)
	if err != nil {
		return "", "", err
	}

	selected := make(map[string]struct{}, len(selection.Files))
	for _, file := range selection.Files {
		selected[file.Path] = struct{}{}
	}
	var lines strings.Builder
	var patchPaths []string
	pathBytes := 0
	for _, file := range files {
		if _, ok := selected[file.Path]; !ok && !selection.All {
			continue
		}
		lines.WriteString(file.Status)
		if file.OldPath != "" {
			lines.WriteString("\t" + file.OldPath)
		}
		lines.WriteString("\t" + file.Path + "\n")
		if pathBytes += len(file.OldPath) + len(file.Path); pathBytes <= commitPatchPathBytes || len(patchPaths) == 0 {
			if file.OldPath != "" {
				patchPaths = append(patchPaths, file.OldPath)
			}
			patchPaths = append(patchPaths, file.Path)
		}
	}
	if lines.Len() == 0 {
		return "", "", nil
	}

	diff := snapshot.Diff(gitdiff.Options{})
	if !selection.All {
		if diff, err = snapshot.FilesDiff(patchPaths, gitdiff.Options{}); err != nil {
			return "", "", err
		}
	}
	chunk, err := diff.Read(ctx, 0, commitmsg.PromptStagedPatchLimit)
	if err = errors.Join(err, diff.Close()); err != nil {
		return "", "", err
	}
	return lines.String(), chunk.Data, nil
}
