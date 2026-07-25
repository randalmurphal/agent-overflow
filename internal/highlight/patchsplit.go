package highlight

import "strings"

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
//     "+++ " lines (unless /dev/null);
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
	path := ""
	finish := func() {
		if len(cur) > 0 && path != "" {
			out = append(out, PatchFileSeed{Path: path, Patch: strings.Join(cur, "\n")})
		}
		cur = nil
		path = ""
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			finish()
			parts := strings.Fields(line)
			if len(parts) > 3 {
				path = cleanPatchPath(parts[3])
			}
			cur = []string{line}
			continue
		}
		if cur == nil {
			continue
		}
		// Mirrors parsePatchFiles' unconditional assignment: a bogus
		// empty "rename to" clears the path there too.
		if rest, ok := strings.CutPrefix(line, "rename to "); ok {
			path = cleanPatchPath(rest)
		}
		if rest, ok := strings.CutPrefix(line, "+++ "); ok {
			if next := cleanPatchPath(rest); next != "" && next != "/dev/null" {
				path = next
			}
		}
		cur = append(cur, line)
	}
	finish()
	return out
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
