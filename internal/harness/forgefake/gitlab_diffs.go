package forgefake

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A GitLab merge request's diffs
// (https://docs.gitlab.com/api/merge_requests/#list-merge-request-diffs)
// are its Diff split into files. A position names a file by both paths and
// a line by the diff's cursors: GitLab refuses a file or a line the diff
// does not have, and a line_range point's line code is sha1(new path),
// then the old and the new cursor at that line (an added line's old cursor
// is the next old line, a removed line's new cursor the next new line).

// gitlabDiffFile is one file of a pull's Diff.
type gitlabDiffFile struct {
	oldPath, newPath          string
	newFile, renamed, deleted bool
	hunks                     string
	added, removed, unchanged map[int]int
	ranges                    []gitlabDiffHunk
}

// gitlabDiffHunk is a hunk's first lines and the cursors after its last.
type gitlabDiffHunk struct{ oldStart, newStart, oldNext, newNext int }

var gitlabFakeHunk = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// gitlabDiffFiles splits a unified git diff into its files. A new file
// names its new path twice and a deleted one its old path twice, as
// GitLab answers them.
func gitlabDiffFiles(diff string) []gitlabDiffFile {
	var out []gitlabDiffFile
	var current *gitlabDiffFile
	var hunks strings.Builder
	oldLine, newLine := 0, 0
	finish := func() {
		if current == nil {
			return
		}
		current.hunks = hunks.String()
		if current.newFile {
			current.oldPath = current.newPath
		}
		if current.deleted {
			current.newPath = current.oldPath
		}
		out = append(out, *current)
		current = nil
		hunks.Reset()
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			finish()
			current = &gitlabDiffFile{added: map[int]int{}, removed: map[int]int{}, unchanged: map[int]int{}}
			if a, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
				current.oldPath, current.newPath = strings.TrimPrefix(a, "a/"), b
			}
		case current == nil:
		case len(current.ranges) == 0 && strings.HasPrefix(line, "rename from "):
			current.oldPath, current.renamed = strings.TrimPrefix(line, "rename from "), true
		case len(current.ranges) == 0 && strings.HasPrefix(line, "rename to "):
			current.newPath, current.renamed = strings.TrimPrefix(line, "rename to "), true
		case len(current.ranges) == 0 && strings.HasPrefix(line, "new file mode"):
			current.newFile = true
		case len(current.ranges) == 0 && strings.HasPrefix(line, "deleted file mode"):
			current.deleted = true
		case len(current.ranges) == 0 && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "index ")):
		default:
			if m := gitlabFakeHunk.FindStringSubmatch(line); m != nil {
				oldLine, _ = strconv.Atoi(m[1])
				newLine, _ = strconv.Atoi(m[2])
				current.ranges = append(current.ranges, gitlabDiffHunk{oldLine, newLine, oldLine, newLine})
			} else if len(current.ranges) == 0 || line == "" {
				continue
			} else {
				switch line[0] {
				case '+':
					current.added[newLine] = oldLine
					newLine++
				case '-':
					current.removed[oldLine] = newLine
					oldLine++
				case ' ':
					current.unchanged[newLine] = oldLine
					oldLine++
					newLine++
				default:
					// "\ No newline at end of file" moves neither cursor.
				}
				last := &current.ranges[len(current.ranges)-1]
				last.oldNext, last.newNext = oldLine, newLine
			}
			hunks.WriteString(line)
			hunks.WriteByte('\n')
		}
	}
	finish()
	return out
}

// gitlabDiffFileOf is the pull's diff file whose new path is path.
func gitlabDiffFileOf(p *Pull, path string) (gitlabDiffFile, bool) {
	for _, file := range gitlabDiffFiles(p.Diff) {
		if file.newPath == path {
			return file, true
		}
	}
	return gitlabDiffFile{}, false
}

// cursors are the old and new cursor at a line: oldLine only names a
// removed line, newLine only an added one, both an unchanged one (inside
// a hunk, or outside every hunk at the offset of the gap it is in).
func (f gitlabDiffFile) cursors(oldLine, newLine *int) (int, int, bool) {
	switch {
	case oldLine != nil && newLine != nil:
		old, ok := f.unchanged[*newLine]
		if !ok {
			old, ok = f.gapOld(*newLine)
		}
		return *oldLine, *newLine, ok && old == *oldLine
	case oldLine != nil:
		next, ok := f.removed[*oldLine]
		return *oldLine, next, ok
	case newLine != nil:
		old, ok := f.added[*newLine]
		return old, *newLine, ok
	}
	return 0, 0, false
}

func (f gitlabDiffFile) gapOld(line int) (int, bool) {
	if line <= 0 {
		return 0, false
	}
	offset := 0
	for _, hunk := range f.ranges {
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

func (f gitlabDiffFile) lineCode(oldLine, newLine int) string {
	return fmt.Sprintf("%x_%d_%d", sha1.Sum([]byte(f.newPath)), oldLine, newLine)
}

func (f gitlabDiffFile) json() map[string]any {
	return map[string]any{
		"old_path": f.oldPath, "new_path": f.newPath, "diff": f.hunks,
		"new_file": f.newFile, "renamed_file": f.renamed, "deleted_file": f.deleted,
		"collapsed": false, "too_large": false, "a_mode": "100644", "b_mode": "100644",
	}
}

// glabMRDiffs answers `GET merge_requests/:iid/diffs`: the pull's Diff by
// file, paginated by per_page (20 by default) and page.
func glabMRDiffs(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	files := gitlabDiffFiles(p.Diff)
	items := make([]map[string]any, 0, len(files))
	for _, file := range files {
		items = append(items, file.json())
	}
	items, next := paginate(items, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// gitlabPositionInvalid is GitLab's answer to a position its diff does not
// have.
func gitlabPositionInvalid() response {
	return glabStatus(http.StatusBadRequest, "Note {:line_code=>[\"can't be blank\", \"must be a valid line code\"]}")
}
