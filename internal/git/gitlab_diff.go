package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// A GitLab position names a file by both its paths and, in a line range,
// each end by a line code: sha1 of the file's path, then the old and the
// new line the diff's two cursors are at on that line (an added line's
// old cursor is the next old line, a removed line's new cursor the next
// new line). The caller names a file by one path and a line by its own
// number, so a positioned write reads the merge request's diffs (the
// diff GitLab positions against) for each file it names: both paths and
// both cursors of every line, no line of which the diff lacks.

// gitlabDiffPageSize and gitlabDiffMaxPages bound the read: 100 files a
// page, at most 30 pages before a file the caller names counts as absent.
const (
	gitlabDiffPageSize = 100
	gitlabDiffMaxPages = 30
	gitlabDiffMaxBody  = 8 << 20
)

// gitlabDiffFile is one file of the merge request's diff as a position
// needs it.
type gitlabDiffFile struct {
	oldPath, newPath string
	// lines is false for a diff GitLab does not show (collapsed or too
	// large): the file takes a file comment and no line comment.
	lines bool
	// added maps an added line's new number to the old cursor, removed a
	// removed line's old number to the new cursor, context an unchanged
	// line's new number to its old one.
	added, removed, context map[int]int
	// hunks are the hunks' first lines and the cursors after their last,
	// in order: an unchanged line outside every hunk (expanded context)
	// keeps the offset of the gap it is in.
	hunks []gitlabHunk
}

type gitlabHunk struct {
	oldStart, newStart, oldNext, newNext int
}

type gitlabDiffRaw struct {
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	Diff      string `json:"diff"`
	Collapsed bool   `json:"collapsed"`
	TooLarge  bool   `json:"too_large"`
}

// readGitLabDiffFiles reads the merge request's diffs and returns, by the
// path the caller named, each path's file: the entry whose new path it is,
// or, only when no entry has it as its new path, the entry whose old path
// it is (a file renamed away). A path that is a new path stops the read
// once all are found; an old-path alias holds only after every page that
// could hold the path as a new path was read. A path the diff lacks is an
// error naming it.
func readGitLabDiffFiles(ctx context.Context, client *forgeapi.Client, ref PRReference, paths []string) (map[string]gitlabDiffFile, error) {
	want := map[string]bool{}
	for _, path := range paths {
		want[path] = true
	}
	out := make(map[string]gitlabDiffFile, len(want))
	aliases := map[string]gitlabDiffFile{}
	request := forgeapi.Request{
		Path:         gitlabMRPath(ref) + "/diffs",
		Query:        url.Values{"per_page": {strconv.Itoa(gitlabDiffPageSize)}},
		MaxBodyBytes: gitlabDiffMaxBody,
	}
	page, complete := 1, false
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var files []gitlabDiffRaw
		if err := json.Unmarshal(resp.Body, &files); err != nil {
			return false, fmt.Errorf("GitLab merge request diffs: decode response: %w", err)
		}
		for _, raw := range files {
			if want[raw.NewPath] {
				out[raw.NewPath] = parseGitLabDiffFile(raw)
			}
			if _, seen := aliases[raw.OldPath]; want[raw.OldPath] && raw.OldPath != raw.NewPath && !seen {
				aliases[raw.OldPath] = parseGitLabDiffFile(raw)
			}
		}
		if len(out) == len(want) {
			return false, nil
		}
		if !advanceGitLabPage(resp, &page) {
			complete = true
			return false, nil
		}
		return page <= gitlabDiffMaxPages, nil
	})
	if err != nil {
		return nil, err
	}
	for path := range want {
		if _, ok := out[path]; ok {
			continue
		}
		if alias, ok := aliases[path]; ok && complete {
			out[path] = alias
			continue
		}
		return nil, fmt.Errorf("%s is not in the merge request's diff", path)
	}
	return out, nil
}

var gitlabHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseGitLabDiffFile walks the file's hunks as GitLab's diff parser does,
// keeping both cursors on every line.
func parseGitLabDiffFile(raw gitlabDiffRaw) gitlabDiffFile {
	file := gitlabDiffFile{
		oldPath: raw.OldPath, newPath: raw.NewPath,
		lines: !raw.Collapsed && !raw.TooLarge && raw.Diff != "",
		added: map[int]int{}, removed: map[int]int{}, context: map[int]int{},
	}
	oldLine, newLine, inHunk := 0, 0, false
	for _, line := range strings.Split(raw.Diff, "\n") {
		if m := gitlabHunkHeader.FindStringSubmatch(line); m != nil {
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			file.hunks = append(file.hunks, gitlabHunk{oldStart: oldLine, newStart: newLine, oldNext: oldLine, newNext: newLine})
			inHunk = true
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			file.added[newLine] = oldLine
			newLine++
		case '-':
			file.removed[oldLine] = newLine
			oldLine++
		case ' ':
			file.context[newLine] = oldLine
			oldLine++
			newLine++
		default:
			continue
		}
		last := &file.hunks[len(file.hunks)-1]
		last.oldNext, last.newNext = oldLine, newLine
	}
	return file
}

// unchangedOld is the old number of the unchanged line at new number line
// outside every hunk, from the offset of the gap it is in; false for a
// line inside a hunk.
func (f gitlabDiffFile) unchangedOld(line int) (int, bool) {
	offset := 0
	for _, hunk := range f.hunks {
		if line < hunk.newStart {
			return line + hunk.oldStart - hunk.newStart, true
		}
		if line < hunk.newNext {
			return 0, false
		}
		offset = hunk.oldNext - hunk.newNext
	}
	return line + offset, true
}

// point is one line of the file as GitLab names it in a line range: a
// removed line by its old number (type old), an added line by its new
// number (type new), an unchanged line on the right by both (no type,
// https://docs.gitlab.com/api/discussions/#create-a-new-thread-in-the-merge-request-diff),
// each with the line code of both cursors. A line the diff does not have
// is an error.
func (f gitlabDiffFile) point(line, oldLine int, side string) (gitlabLineRangePoint, error) {
	missing := fmt.Errorf("line %d of %s is not in the merge request's diff", line, f.newPath)
	if !f.lines {
		return gitlabLineRangePoint{}, fmt.Errorf("GitLab does not show the diff of %s, so a comment cannot name its lines", f.newPath)
	}
	if line <= 0 || oldLine < 0 {
		return gitlabLineRangePoint{}, missing
	}
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "left", "old":
		next, ok := f.removed[line]
		if !ok || oldLine != 0 {
			return gitlabLineRangePoint{}, missing
		}
		return gitlabLineRangePoint{LineCode: gitlabLineCode(f.newPath, line, next), Type: "old", OldLine: &line}, nil
	case "right", "new", "":
		if oldLine > 0 {
			old, ok := f.context[line]
			if !ok {
				old, ok = f.unchangedOld(line)
			}
			if !ok || old != oldLine {
				return gitlabLineRangePoint{}, missing
			}
			return gitlabLineRangePoint{LineCode: gitlabLineCode(f.newPath, oldLine, line), OldLine: &oldLine, NewLine: &line}, nil
		}
		old, ok := f.added[line]
		if !ok {
			return gitlabLineRangePoint{}, missing
		}
		return gitlabLineRangePoint{LineCode: gitlabLineCode(f.newPath, old, line), Type: "new", NewLine: &line}, nil
	}
	return gitlabLineRangePoint{}, fmt.Errorf("a comment on %s has invalid side %q", f.newPath, side)
}

// gitlabPositionedPaths are the files the comments' positions name.
func gitlabPositionedPaths(comments []ReviewLineComment) []string {
	var paths []string
	for _, comment := range comments {
		if comment.InReplyTo == nil && !strings.EqualFold(comment.Side, "summary") && strings.TrimSpace(comment.Path) != "" {
			paths = append(paths, comment.Path)
		}
	}
	return paths
}
