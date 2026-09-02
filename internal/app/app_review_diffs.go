package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/workspacepath"
)

// reviewWorkspace resolves a review RPC's caller-supplied workspace ref
// through the one resolver, prefixed with the caller's action so the
// frontend's userFacingError shows which read was refused.
func (a *App) reviewWorkspace(action string, ws WorkspaceRef) (string, error) {
	_, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return workspace, nil
}

// GetWorkspaceCurrentDiff returns the unified patch of everything
// currently uncommitted in the referenced workspace (tracked changes
// against HEAD plus untracked-not-ignored files). Empty for non-git
// workspaces.
//
// ignoreWhitespace is the review pane's "hide whitespace changes"
// toggle (`-w`); see gitdiff.Options.
func (a *App) GetWorkspaceCurrentDiff(ws WorkspaceRef, ignoreWhitespace bool) (string, error) {
	const action = "get workspace current diff"
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return "", err
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := gitdiff.DiffWorkspaceVsHead(context.Background(), workspace,
		gitdiff.Options{IgnoreWhitespace: ignoreWhitespace})
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// GetBranchBaseDiff returns the combined diff of the referenced workspace
// (committed work since merge-base plus uncommitted changes) against the
// merge base of baseBranch and the workspace HEAD — i.e. what a PR onto
// baseBranch would contain.
func (a *App) GetBranchBaseDiff(ws WorkspaceRef, baseBranch string, ignoreWhitespace bool) (string, error) {
	const action = "get branch base diff"
	if strings.TrimSpace(baseBranch) == "" {
		return "", fmt.Errorf("%s: base branch is required", action)
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return "", err
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := gitdiff.DiffBranchBaseToWorktree(context.Background(), workspace, baseBranch,
		gitdiff.Options{IgnoreWhitespace: ignoreWhitespace})
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// BranchCommit is the wire shape of one row in the review pane's
// per-commit selector.
type BranchCommit = gitdiff.Commit

// ListBranchCommits returns the commits a PR from the workspace HEAD
// onto baseBranch would carry (`base..HEAD`, newest first). Empty for
// non-git workspaces.
func (a *App) ListBranchCommits(ws WorkspaceRef, baseBranch string) ([]BranchCommit, error) {
	const action = "list branch commits"
	if strings.TrimSpace(baseBranch) == "" {
		return nil, fmt.Errorf("%s: base branch is required", action)
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return nil, err
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return []BranchCommit{}, nil
	}
	commits, err := gitdiff.ListCommits(context.Background(), workspace, baseBranch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return commits, nil
}

// recentCommitLimit matches codex's own "Review a commit" picker
// (recent_commits(cwd, 100)); the `/review` completion mirrors it.
const recentCommitLimit = 100

// ListRecentCommits returns the workspace's most recent commits (plain
// `git log` from HEAD, newest first) — the same source codex's own
// review picker uses, so a workspace on the default branch still gets a
// list. Empty for non-git workspaces.
func (a *App) ListRecentCommits(ws WorkspaceRef) ([]BranchCommit, error) {
	const action = "list recent commits"
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return nil, err
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return []BranchCommit{}, nil
	}
	commits, err := gitdiff.ListRecentCommits(context.Background(), workspace, recentCommitLimit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return commits, nil
}

// GetCommitDiff returns the unified patch a single local commit
// introduced (first-parent diff; empty-tree diff for a root commit).
func (a *App) GetCommitDiff(ws WorkspaceRef, sha string, ignoreWhitespace bool) (string, error) {
	const action = "get commit diff"
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return "", err
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", fmt.Errorf("%s: workspace is not a git repository", action)
	}
	patch, err := gitdiff.CommitDiff(context.Background(), workspace, sha,
		gitdiff.Options{IgnoreWhitespace: ignoreWhitespace})
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// DiffContextRequest identifies one hunk-gap slice of a review diff's
// NEW side (expanded context is unchanged on both sides, so the new
// side is the only source needed). Scope mirrors the review pane's
// scope selector; the extra fields disambiguate the new-side source
// where the scope alone can't: CommitSHA for commit scope (a selected
// commit in the workspace repo), HeadSHA for pr scope (the fetched
// head commit).
type DiffContextRequest struct {
	Scope     string `json:"scope"`
	CommitSHA string `json:"commitSHA"`
	HeadSHA   string `json:"headSHA"`
	Path      string `json:"path"`
	// 1-based inclusive line range on the new side.
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
	// VerifyPatch (edits scope only): the file's historical patch text.
	// A snapshot or workspace file is served only when every new-side
	// patch line still byte-matches it — a drifted file must never
	// masquerade as historical context. Live scopes ignore the field
	// (their content IS the diff's source by construction).
	VerifyPatch string `json:"verifyPatch,omitempty"`
	// Edit selection (edits scope only): which edit's persisted file
	// snapshot resolves this path. EditPayloadID selects one edit's
	// snapshot; when it is empty EditTurnIndex selects the whole turn,
	// whose LAST snapshot of Path is the state the merged section
	// describes. Snapshots are still verified against VerifyPatch, and
	// an absent snapshot (pre-feature history) falls back to workspace
	// verification.
	EditPayloadID string `json:"editPayloadId"`
	EditTurnIndex int    `json:"editTurnIndex"`
}

type DiffContextResult struct {
	Lines     []string `json:"lines"`
	StartLine int      `json:"startLine"`
	// EOF: the range reached (or passed) the file's last line, so the
	// frontend can retire a trailing gap whose size it couldn't know.
	EOF        bool `json:"eof"`
	TotalLines int  `json:"totalLines"`
}

// One expansion click fetches at most this many lines; "expand all"
// on pathological gaps must step instead of shipping a whole file.
const maxDiffContextLines = 1000

// GetDiffContextLines returns new-side source lines for review-diff
// hunk-gap expansion in the LIVE scopes — workspace, branch, commit and pr —
// whose new side is content the referenced checkout already holds. The edits
// scope is a different subject (one thread's own history) and has its own
// entry point. Same wire-exposure class as the diff getters: classified
// LocalOnlyMethods.
func (a *App) GetDiffContextLines(ws WorkspaceRef, req DiffContextRequest) (DiffContextResult, error) {
	const action = "get diff context lines"
	if a.shuttingDown.Load() {
		return DiffContextResult{}, ErrShuttingDown
	}
	workspace, err := a.liveDiffWorkspace(action, ws, req.Scope)
	if err != nil {
		return DiffContextResult{}, err
	}
	return a.diffContextLines(action, workspace, "", req)
}

// GetEditDiffContextLines serves the edits scope, whose new side is a
// historical file state: the snapshot persisted with the edit, falling back to
// the THREAD's own workspace for edits that predate snapshots. Both sources
// are verified against the request's patch before a line is served.
func (a *App) GetEditDiffContextLines(threadID string, req DiffContextRequest) (DiffContextResult, error) {
	const action = "get edit diff context lines"
	if a.shuttingDown.Load() {
		return DiffContextResult{}, ErrShuttingDown
	}
	if req.Scope != "edits" {
		return DiffContextResult{}, fmt.Errorf("%s: scope %q is not an edits scope", action, req.Scope)
	}
	workspace, err := a.threadDiffWorkspace(action, threadID)
	if err != nil {
		return DiffContextResult{}, err
	}
	return a.diffContextLines(action, workspace, threadID, req)
}

// liveDiffWorkspace resolves the checkout a live review scope reads from. pr
// is the one scope with a second answer: a pr-anchor thread carries no local
// clone, and its callers get "requires a local clone" rather than a project
// resolution failure.
func (a *App) liveDiffWorkspace(action string, ws WorkspaceRef, scope string) (string, error) {
	switch scope {
	case "workspace", "branch", "commit":
		return a.reviewWorkspace(action, ws)
	case "pr":
		workspace, ok := a.localCloneWorkspace(ws)
		if !ok {
			return "", errors.New("expanding context requires a local clone")
		}
		return workspace, nil
	default:
		return "", fmt.Errorf("%s: %q is not a live review scope", action, scope)
	}
}

// threadDiffWorkspace resolves the checkout a THREAD-keyed diff read uses: its
// own workspace, off its own row. Thread-keyed on purpose — an edit happened
// in whatever checkout the thread occupied, and only the row knows which — and
// deliberately not a path-validation path: nothing here comes from a caller.
func (a *App) threadDiffWorkspace(action, threadID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return workspace, nil
}

func (a *App) diffContextLines(action, workspace, threadID string, req DiffContextRequest) (DiffContextResult, error) {
	if req.StartLine < 1 || req.EndLine < req.StartLine {
		return DiffContextResult{}, fmt.Errorf("%s: invalid line range %d-%d", action, req.StartLine, req.EndLine)
	}
	if req.EndLine-req.StartLine+1 > maxDiffContextLines {
		return DiffContextResult{}, fmt.Errorf("%s: range exceeds %d lines", action, maxDiffContextLines)
	}
	content, tabExpanded, err := a.diffContextContent(action, workspace, threadID, req, 0)
	if err != nil {
		return DiffContextResult{}, err
	}
	lines := splitContentLines(content)
	total := len(lines)
	if req.StartLine > total {
		return DiffContextResult{Lines: []string{}, StartLine: req.StartLine, EOF: true, TotalLines: total}, nil
	}
	end := min(req.EndLine, total)
	served := append([]string(nil), lines[req.StartLine-1:end]...)
	if tabExpanded {
		// The verified patch carries Claude's tab mangling (leading tabs
		// shipped as two spaces); served lines sit between its hunk lines,
		// so they get the same transform or the indentation visibly jumps.
		for i, line := range served {
			served[i] = highlight.ExpandLeadingTabs(line)
		}
	}
	return DiffContextResult{
		Lines:      served,
		StartLine:  req.StartLine,
		EOF:        end >= total,
		TotalLines: total,
	}, nil
}

// VerifyEditDiffsRequest carries one edits-scope load's candidate files
// for batch expandability verification. The edit selection mirrors
// DiffContextRequest; each file's VerifyPatch is its merged historical
// patch text.
type VerifyEditDiffsRequest struct {
	EditPayloadID string               `json:"editPayloadId"`
	EditTurnIndex int                  `json:"editTurnIndex"`
	Files         []EditDiffVerifyFile `json:"files"`
}

// EditDiffVerifyFile is one candidate file of a VerifyEditDiffs batch.
type EditDiffVerifyFile struct {
	Path        string `json:"path"`
	VerifyPatch string `json:"verifyPatch"`
}

// VerifyEditDiffsResult lists the paths whose expansion requests would
// be served — the positive gate for rendering gap arrows.
type VerifyEditDiffsResult struct {
	ExpandablePaths []string `json:"expandablePaths"`
}

// maxVerifyEditDiffFiles bounds one verification batch; overflow files
// simply stay unexpandable (fail-closed keeps arrows honest).
const maxVerifyEditDiffFiles = 200

// VerifyEditDiffs reports which of an edits-scope diff's files can
// serve hunk-gap expansion, so the frontend renders arrows only where a
// click would succeed. Each file runs the SAME resolution the serving
// path uses (snapshot first, workspace fallback, verified against the
// patch either way), bounded at MaxPrimeBytes per file so one giant
// generated file can't force unbounded reads on every load — the one
// deliberate divergence from click-time resolution, and it only errs
// fail-closed: an over-cap file shows no arrow (snapshots never exceed
// the cap either; only a huge pre-snapshot workspace file can hit it).
// Same wire-exposure class as GetDiffContextLines: classified
// LocalOnlyMethods; remote clients' rejection leaves every path
// unexpandable, which is exactly what their clicks would find.
func (a *App) VerifyEditDiffs(threadID string, req VerifyEditDiffsRequest) (VerifyEditDiffsResult, error) {
	const action = "verify edit diffs"
	if a.shuttingDown.Load() {
		return VerifyEditDiffsResult{}, ErrShuttingDown
	}
	files := req.Files
	if len(files) > maxVerifyEditDiffFiles {
		files = files[:maxVerifyEditDiffFiles]
	}
	workspace, err := a.threadDiffWorkspace(action, threadID)
	if err != nil {
		return VerifyEditDiffsResult{}, err
	}
	expandable := []string{}
	for _, file := range files {
		_, _, err := a.diffContextContent(action, workspace, threadID, DiffContextRequest{
			Scope:         "edits",
			Path:          file.Path,
			VerifyPatch:   file.VerifyPatch,
			EditPayloadID: req.EditPayloadID,
			EditTurnIndex: req.EditTurnIndex,
		}, highlight.MaxPrimeBytes)
		if err == nil {
			expandable = append(expandable, file.Path)
		}
	}
	return VerifyEditDiffsResult{ExpandablePaths: expandable}, nil
}

// diffContextContent resolves the new-side file content for a diff scope in
// an ALREADY-resolved workspace: the entry point owns "which checkout", this
// owns "where in it". threadID is set only for the edits scope, whose
// snapshots are keyed on the thread.
//
// maxBytes > 0 rejects oversized files — workspace scopes do a
// descriptor-bounded read (readWorkspaceFile); ref scopes read the
// blob and length-check it (git plumbing has no cheap pre-read size
// probe worth an extra process). tabExpanded is edits-scope only: the
// verification patch matched via ExpandLeadingTabs, so lines served
// beside its hunks need the same transform.
func (a *App) diffContextContent(action, workspace, threadID string, req DiffContextRequest, maxBytes int64) (content string, tabExpanded bool, err error) {
	switch req.Scope {
	case "workspace", "branch":
		// These scopes diff against the working tree — the new side is
		// the file on disk, uncommitted edits included.
		rel, err := workspacepath.NormalizeRelative(req.Path)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", action, err)
		}
		content, err := readWorkspaceFile(filepath.Join(workspace, rel), maxBytes)
		if err != nil {
			return "", false, fmt.Errorf("%s: %s: %w", action, rel, err)
		}
		return content, false, nil
	case "commit":
		// Commit diffs are parent → commit; the new side is the
		// selected commit's tree, which can lag the worktree.
		sha := strings.TrimSpace(req.CommitSHA)
		if sha == "" {
			return "", false, fmt.Errorf("%s: commit SHA is required", action)
		}
		content, err := gitdiff.ShowFileAtCommit(context.Background(), workspace, sha, req.Path)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", action, err)
		}
		return capContent(action, req.Path, content, maxBytes)
	case "pr":
		// The fetched head commit is present locally after GetPRDiff's
		// FetchRefOID (and with it every commit the PR carries). A selected
		// per-commit diff reads that commit's tree instead of the head.
		sha := strings.TrimSpace(req.CommitSHA)
		if sha == "" {
			sha = strings.TrimSpace(req.HeadSHA)
		}
		if sha == "" {
			return "", false, fmt.Errorf("%s: head SHA is required", action)
		}
		content, err := a.gitCore().ShowTreeFile(workspace, sha, req.Path)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", action, err)
		}
		return capContent(action, req.Path, content, maxBytes)
	case "edits":
		// An edit diff is historical: only its hunks were persisted as
		// the patch, and no tree from that moment exists. Resolution is
		// snapshot-first — the file content captured at persist time,
		// when it provably matched the patch — with the current
		// workspace file as the fallback for edits that predate
		// snapshots. Either source is served ONLY when it still matches
		// the patch (every new-side hunk line equal at its line number,
		// exactly or modulo Claude's leading-tab mangling) — otherwise
		// refuse, and the frontend disables the file's expansion
		// affordances. Default-closed: no VerifyPatch or an oversized
		// one refuses too.
		if req.VerifyPatch == "" || len(req.VerifyPatch) > highlight.MaxRequestBytes {
			return "", false, fmt.Errorf("%s: edit diff verification patch is required", action)
		}
		if content, ok := a.editFileSnapshot(threadID, req); ok &&
			(maxBytes <= 0 || int64(len(content)) <= maxBytes) {
			if matched, expanded := highlight.PatchContentMatch(req.VerifyPatch, content); matched {
				return content, expanded, nil
			}
		}
		rel, err := workspacepath.NormalizeRelative(req.Path)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", action, err)
		}
		content, err := readWorkspaceFile(filepath.Join(workspace, rel), maxBytes)
		if err != nil {
			return "", false, fmt.Errorf("%s: %s: %w", action, rel, err)
		}
		matched, expanded := highlight.PatchContentMatch(req.VerifyPatch, content)
		if !matched {
			return "", false, fmt.Errorf("%s: %s has changed since this edit", action, rel)
		}
		return content, expanded, nil
	default:
		return "", false, fmt.Errorf("%s: unknown scope %q", action, req.Scope)
	}
}

// editFileSnapshot resolves the persisted new-side snapshot for the
// request's edit selection. Store read errors log and report no
// snapshot — the workspace fallback may still verify, and refusing
// expansion over a cache-read hiccup would retire arrows needlessly.
func (a *App) editFileSnapshot(threadID string, req DiffContextRequest) (string, bool) {
	if req.EditPayloadID != "" {
		content, found, err := a.store.GetEditFileSnapshot(threadID, req.EditPayloadID, req.Path)
		if err != nil {
			log.Printf("app: read edit file snapshot %s %s: %v", req.EditPayloadID, req.Path, err)
			return "", false
		}
		return content, found
	}
	content, found, err := a.store.GetLatestTurnEditFileSnapshot(threadID, req.EditTurnIndex, req.Path)
	if err != nil {
		log.Printf("app: read turn edit file snapshot %s/%d %s: %v", threadID, req.EditTurnIndex, req.Path, err)
		return "", false
	}
	return content, found
}

func capContent(action, path, content string, maxBytes int64) (string, bool, error) {
	if maxBytes > 0 && int64(len(content)) > maxBytes {
		return "", false, fmt.Errorf("%s: %s exceeds %d bytes", action, path, maxBytes)
	}
	return content, false, nil
}

// readWorkspaceFile reads a workspace file that a coding agent may be
// mutating concurrently. All checks run on the open descriptor —
// O_NONBLOCK keeps the open itself from hanging if the path is a FIFO,
// fstat classifies what was actually opened (a pre-open stat could
// pass a file that is swapped before the read), and the bounded read
// caps allocation even when the file grows after the fstat. maxBytes
// <= 0 means unbounded (the type check still applies).
func readWorkspaceFile(path string, maxBytes int64) (string, error) {
	data, err := readWorkspaceFileBytes(path, maxBytes)
	return string(data), err
}

func readWorkspaceFileBytes(path string, maxBytes int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if maxBytes <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return data, nil
}

// splitContentLines splits file content into lines the way diff line
// numbering counts them: a trailing newline terminates the last line
// rather than opening an empty one.
func splitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	content = strings.TrimSuffix(content, "\n")
	return strings.Split(content, "\n")
}
