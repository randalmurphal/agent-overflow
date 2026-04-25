// Pure utility: extract file-path references from a free-text string.
//
// The shapes we want to match are:
//   path/to/file.ext
//   path/to/file.ext:42
//   path/to/file.ext:42:7
//   ./relative.ts
//   ../parent.ts
//   /Users/me/abs.ts
//
// Things we deliberately do NOT match (these would otherwise produce
// noisy false positives in normal prose):
//   - URLs (`https://...`, `http://...`, `ftp://...`)
//   - Scoped npm packages (`@scope/pkg`)
//   - Email addresses (`user@example.com`)
//   - Bare module names without an extension (`marked`, `vitest`)
//
// The exclusions are anchored by the leading boundary in the regex:
// the path must start at the input boundary or after a "safe" character
// (whitespace, parenthesis, bracket, comma, etc.) — and never directly
// after `:`, `/`, `@`, or alphanumerics, which is enough to filter out
// the shapes above without paying the cost of full URL parsing.
//
// The output is a list of byte-offset ranges into the input string;
// callers can use them to walk the original text, replacing matched
// substrings with linkified anchors. We return everything sorted by
// `start` so consumers can stitch together "between-match" plain text
// in a single pass.

/**
 * One linkified path range.
 */
export interface PathRange {
  /** Inclusive start offset into the input string. */
  start: number;
  /** Exclusive end offset into the input string. */
  end: number;
  /** The path token without the optional `:line:col` suffix. */
  path: string;
  /** 1-indexed line number, or undefined when no line was matched. */
  line?: number;
  /** 1-indexed column, or undefined when no column was matched. */
  col?: number;
}

// Path body: at least one path-segment. Allow letters, digits, `_`,
// `-`, `.`, `~`, `/`, `\\` — `:` is intentionally excluded from the
// body because it terminates the path (start of `:line:col` suffix).
//
// `(?:^|(?<=[\\s(\\[{,;'"\`<>=]))` anchors a leading boundary that's
// either the input start or one of the characters typical text
// surrounding a path. The negative lookbehind cases (`/`, `@`, `:`,
// alphanumeric) are absent from this set, which prevents matching the
// tail of `https://example.com/foo` (the `e` before `x` is alphanumeric)
// and `@scope/pkg` (the `@` is the immediately-preceding char).
const PATH_PATTERN =
  /(?:^|(?<=[\s(\[{,;'"`<>=]))((?:\.{0,2}\/|\/)?[\w.\-~]+(?:\/[\w.\-~]+)+)(?::(\d+)(?::(\d+))?)?/g;

// At least one of these segment shapes must appear so we don't linkify
// trivial-looking strings. The patterns above already require >=1 `/`,
// but a path that starts with neither `./`, `../`, nor `/` AND only
// contains `[\w.-]` is treated as a relative path candidate (e.g.
// `src/lib/foo.ts`). We further require the final segment to contain
// at least one `.` so we keep noise like `path/to/dir` from getting
// linkified — the brief calls out file-path references, not arbitrary
// directory tokens.
//
// Bare import names like `marked` or `vitest` are filtered out by this
// final-segment-with-dot rule because they don't contain `/`.
function looksLikeFilePath(token: string): boolean {
  // Reject obvious non-paths up front.
  if (token.includes('//')) return false; // collapses `://` and `..//`
  // Final segment must include a `.` to look like a file. This is what
  // distinguishes `src/lib/foo.ts` from `src/lib`.
  const lastSlash = token.lastIndexOf('/');
  const finalSegment = lastSlash === -1 ? token : token.slice(lastSlash + 1);
  if (!finalSegment.includes('.')) return false;
  // Reject pure version strings that get past the boundary check, e.g.
  // a stray `1.2.3` accidentally adjacent to a slash. These appear in
  // changelogs but aren't files.
  if (/^\d+(?:\.\d+){1,3}$/.test(finalSegment)) return false;
  return true;
}

/**
 * Return every path-range matched in `text`. Results are sorted by
 * `start` ascending and never overlap (the regex `g` flag advances past
 * each match).
 */
export function findPathRanges(text: string): PathRange[] {
  if (!text) return [];
  const out: PathRange[] = [];
  // Reset lastIndex on the shared regex — `g`-flagged patterns are
  // stateful across calls. A fresh start avoids leaking state from
  // a prior caller.
  PATH_PATTERN.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = PATH_PATTERN.exec(text)) !== null) {
    const [, pathToken, lineRaw, colRaw] = match;
    if (!looksLikeFilePath(pathToken)) continue;
    const fullMatch = match[0];
    const suffixLength =
      (lineRaw ? 1 + lineRaw.length : 0) + (colRaw ? 1 + colRaw.length : 0);
    // The leading boundary may be input-start (length 0) or a single
    // safe character consumed via lookbehind. Anchor the path-start
    // by subtracting the path body + optional :line:col suffix from
    // the end of the full match.
    const matchStart = match.index + fullMatch.length - pathToken.length - suffixLength;
    const matchEnd = matchStart + pathToken.length + suffixLength;
    out.push({
      start: matchStart,
      end: matchEnd,
      path: pathToken,
      line: lineRaw ? Number(lineRaw) : undefined,
      col: colRaw ? Number(colRaw) : undefined,
    });
  }
  return out;
}
