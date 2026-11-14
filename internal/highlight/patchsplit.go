package highlight

import (
	"strings"

	"agent-overflow/internal/unidiff"
)

// PatchFileSeed is one file's slice of a (possibly multi-file) unified
// diff, split exactly the way the frontend's parsePatchFiles
// (frontend/src/lib/utils/patchFiles.ts) does: Patch is the byte-exact
// join of the lines that parser would put in the file's `lines` array,
// and Path is the path it would extract. The pairing matters because
// the frontend keys its diff span cache by
// (path, contentKey(joined lines)) — a seed computed here lands in
// that cache only if both halves match; any divergence just misses the
// key and the frontend falls back to its RPC path (fail-safe, never
// misaligned).
type PatchFileSeed struct {
	Path  string
	Patch string
}

// SplitPatchFiles splits a unified diff into per-file seeds, mirroring
// parsePatchFiles:
//   - a file opens at every line starting "diff --git "; content before
//     the first opener is ignored;
//   - the whole input's single trailing empty line (from a trailing
//     "\n") is dropped;
//   - the path starts as the cleaned 4th whitespace-separated token of
//     the opener, then is overwritten by "rename to " lines and by
//     "+++ " lines (unless /dev/null) outside hunk bodies (unidiff.Body);
//   - a file whose path resolves empty is dropped.
func SplitPatchFiles(patch string) []PatchFileSeed {
	if strings.TrimSpace(patch) == "" {
		return nil
	}
	lines := strings.Split(patch, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var out []PatchFileSeed
	var cur []string
	var paths PatchPaths
	finish := func(path string) {
		if len(cur) > 0 && path != "" {
			out = append(out, PatchFileSeed{Path: path, Patch: strings.Join(cur, "\n")})
		}
		cur = nil
	}
	for _, line := range lines {
		if path, closed := paths.Line(line); closed {
			finish(path)
		}
		if paths.open {
			cur = append(cur, line)
		}
	}
	if path, closed := paths.End(); closed {
		finish(path)
	}
	return out
}

// PatchPaths follows a unified diff one line at a time and reports each
// file's path as SplitPatchFiles keys it, holding only the current file's
// path and hunk state, so a patch of any length can be scanned in pieces.
type PatchPaths struct {
	open bool
	path string
	body unidiff.Body
}

// Line takes the next line, without its newline, and answers the path of
// the file it closes: closed is true when line opens another file and a
// file was open (its path may be empty, which SplitPatchFiles drops).
func (p *PatchPaths) Line(line string) (path string, closed bool) {
	if strings.HasPrefix(line, "diff --git ") {
		path, closed = p.path, p.open
		p.open = true
		p.path = ""
		if parts := strings.Fields(line); len(parts) > 3 {
			p.path = cleanPatchPath(parts[3])
		}
		p.body = unidiff.Body{}
		return path, closed
	}
	if !p.open || p.body.Next(line) != unidiff.Outside {
		return "", false
	}
	// Mirrors parsePatchFiles' unconditional assignment: a bogus empty
	// "rename to" clears the path there too.
	if rest, ok := strings.CutPrefix(line, "rename to "); ok {
		p.path = cleanPatchPath(rest)
	}
	if rest, ok := strings.CutPrefix(line, "+++ "); ok {
		if next := cleanPatchPath(rest); next != "" && next != "/dev/null" {
			p.path = next
		}
	}
	return "", false
}

// End answers the path of the file open at the end of the patch.
func (p *PatchPaths) End() (path string, closed bool) {
	path, closed = p.path, p.open
	*p = PatchPaths{}
	return path, closed
}

// cleanPatchPath mirrors the frontend's cleanPath: strip one leading
// and one trailing double quote, then one leading "a/" or "b/".
func cleanPatchPath(raw string) string {
	raw = strings.TrimPrefix(raw, `"`)
	raw = strings.TrimSuffix(raw, `"`)
	if strings.HasPrefix(raw, "a/") || strings.HasPrefix(raw, "b/") {
		raw = raw[2:]
	}
	return raw
}
