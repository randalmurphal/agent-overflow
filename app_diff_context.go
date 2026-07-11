package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/workspacepath"
)

// DiffContextRequest identifies one hunk-gap slice of a review diff's
// NEW side (expanded context is unchanged on both sides, so the new
// side is the only source needed). Scope mirrors the review pane's
// scope selector; the extra fields disambiguate the new-side source
// where the scope alone can't: UserItemID for turn scope (the
// checkpoint the loaded diff targets), HeadSHA for pr scope (the
// fetched head commit).
type DiffContextRequest struct {
	Scope      string `json:"scope"`
	UserItemID string `json:"userItemId"`
	HeadSHA    string `json:"headSHA"`
	Path       string `json:"path"`
	// 1-based inclusive line range on the new side.
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
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
	content, err := a.diffContextContent(action, threadID, req)
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

func (a *App) diffContextContent(action, threadID string, req DiffContextRequest) (string, error) {
	switch req.Scope {
	case "workspace", "branch", "session":
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
		data, err := os.ReadFile(filepath.Join(workspace, rel))
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		return string(data), nil
	case "turn":
		// Turn diffs are checkpoint-ref → checkpoint-ref; the new side
		// is the target checkpoint's commit, which can lag the worktree.
		thread, target, err := a.loadCheckpointForUserItem(action, threadID, req.UserItemID)
		if err != nil {
			return "", err
		}
		workspace, err := checkpoint.ValidateWorkspaceMatch(action, thread.WorkspacePath, target.WorkspacePath)
		if err != nil {
			return "", err
		}
		content, err := a.checkpointStore().ShowFileAtRef(context.Background(), workspace, target.RefName, req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		return content, nil
	case "pr":
		// The fetched head commit is present locally after GetPRDiff's
		// FetchRefOID; API-only PR threads have no clone to read from.
		workspace, ok := a.localCloneWorkspace(threadID)
		if !ok {
			return "", errors.New("expanding context requires a local clone")
		}
		sha := strings.TrimSpace(req.HeadSHA)
		if sha == "" {
			return "", fmt.Errorf("%s: head SHA is required", action)
		}
		content, err := a.gitCore().ShowTreeFile(workspace, sha, req.Path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
		return content, nil
	default:
		return "", fmt.Errorf("%s: unknown scope %q", action, req.Scope)
	}
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
