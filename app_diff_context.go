package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/workspacepath"
)

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
	// The workspace file is served only when every new-side patch line
	// still byte-matches it — a drifted file must never masquerade as
	// historical context. Live scopes ignore the field (their content
	// IS the diff's source by construction).
	VerifyPatch string `json:"verifyPatch,omitempty"`
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
// hunk-gap expansion. Same wire-exposure class as the diff getters:
// classified LocalOnlyMethods.
func (a *App) GetDiffContextLines(threadID string, req DiffContextRequest) (DiffContextResult, error) {
	const action = "get diff context lines"
	if a.shuttingDown.Load() {
		return DiffContextResult{}, ErrShuttingDown
	}
	if req.StartLine < 1 || req.EndLine < req.StartLine {
		return DiffContextResult{}, fmt.Errorf("%s: invalid line range %d-%d", action, req.StartLine, req.EndLine)
	}
	if req.EndLine-req.StartLine+1 > maxDiffContextLines {
		return DiffContextResult{}, fmt.Errorf("%s: range exceeds %d lines", action, maxDiffContextLines)
	}
	content, err := a.diffContextContent(action, threadID, req, 0)
	if err != nil {
		return DiffContextResult{}, err
	}
	lines := splitContentLines(content)
	total := len(lines)
	if req.StartLine > total {
		return DiffContextResult{Lines: []string{}, StartLine: req.StartLine, EOF: true, TotalLines: total}, nil
	}
	end := min(req.EndLine, total)
	return DiffContextResult{
		Lines:      append([]string(nil), lines[req.StartLine-1:end]...),
		StartLine:  req.StartLine,
		EOF:        end >= total,
		TotalLines: total,
	}, nil
}

// diffContextContent resolves the new-side file content for a diff
// scope. maxBytes > 0 rejects oversized files — workspace scopes do a
// descriptor-bounded read (readWorkspaceFile); ref scopes read the
// blob and length-check it (git plumbing has no cheap pre-read size
// probe worth an extra process).
func (a *App) diffContextContent(action, threadID string, req DiffContextRequest, maxBytes int64) (string, error) {
	switch req.Scope {
	case "workspace", "branch":
		// These scopes diff against the working tree — the new side is
		// the file on disk, uncommitted edits included.
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		_, workspace, err := a.resolveGitPaths(thread)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		rel, err := workspacepath.NormalizeRelative(req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		content, err := readWorkspaceFile(filepath.Join(workspace, rel), maxBytes)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", action, rel, err)
		}
		return content, nil
	case "commit":
		// Commit diffs are parent → commit; the new side is the
		// selected commit's tree, which can lag the worktree.
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		_, workspace, err := a.resolveGitPaths(thread)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		sha := strings.TrimSpace(req.CommitSHA)
		if sha == "" {
			return "", fmt.Errorf("%s: commit SHA is required", action)
		}
		content, err := gitdiff.ShowFileAtCommit(context.Background(), workspace, sha, req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		return capContent(action, req.Path, content, maxBytes)
	case "pr":
		// The fetched head commit is present locally after GetPRDiff's
		// FetchRefOID (and with it every commit the PR carries);
		// API-only PR threads have no clone to read from. A selected
		// per-commit diff reads that commit's tree instead of the head.
		workspace, ok := a.localCloneWorkspace(threadID)
		if !ok {
			return "", errors.New("expanding context requires a local clone")
		}
		sha := strings.TrimSpace(req.CommitSHA)
		if sha == "" {
			sha = strings.TrimSpace(req.HeadSHA)
		}
		if sha == "" {
			return "", fmt.Errorf("%s: head SHA is required", action)
		}
		content, err := a.gitCore().ShowTreeFile(workspace, sha, req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		return capContent(action, req.Path, content, maxBytes)
	case "edits":
		// An edit diff is a historical snapshot: only its hunks were
		// persisted, and no tree from that moment exists. The current
		// workspace file stands in ONLY when it provably still matches
		// the patch (every new-side hunk line byte-equal at its line
		// number) — otherwise refuse, and the frontend disables the
		// file's expansion affordances. Default-closed: no VerifyPatch
		// or an oversized one refuses too.
		if req.VerifyPatch == "" || len(req.VerifyPatch) > highlight.MaxRequestBytes {
			return "", fmt.Errorf("%s: edit diff verification patch is required", action)
		}
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		_, workspace, err := a.resolveGitPaths(thread)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		rel, err := workspacepath.NormalizeRelative(req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		content, err := readWorkspaceFile(filepath.Join(workspace, rel), maxBytes)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", action, rel, err)
		}
		if !highlight.PatchMatchesContent(req.VerifyPatch, content) {
			return "", fmt.Errorf("%s: %s has changed since this edit", action, rel)
		}
		return content, nil
	default:
		return "", fmt.Errorf("%s: unknown scope %q", action, req.Scope)
	}
}

func capContent(action, path, content string, maxBytes int64) (string, error) {
	if maxBytes > 0 && int64(len(content)) > maxBytes {
		return "", fmt.Errorf("%s: %s exceeds %d bytes", action, path, maxBytes)
	}
	return content, nil
}

// readWorkspaceFile reads a workspace file that a coding agent may be
// mutating concurrently. All checks run on the open descriptor —
// O_NONBLOCK keeps the open itself from hanging if the path is a FIFO,
// fstat classifies what was actually opened (a pre-open stat could
// pass a file that is swapped before the read), and the bounded read
// caps allocation even when the file grows after the fstat. maxBytes
// <= 0 means unbounded (the type check still applies).
func readWorkspaceFile(path string, maxBytes int64) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if maxBytes <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return string(data), nil
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
