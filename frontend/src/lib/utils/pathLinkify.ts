// Pure utility: extract file-path references from a free-text string.
//
// The canonical path-link pipeline runs in Go:
// `internal/pathlinks.ExtractAndValidate` produces a workspace-validated
// `PathRef[]` allowlist on every surface that renders agent prose
// (assistant_text, channel messages, proposed plans, ask-user-question
// rows, advisor rows). The allowlist rides on `item.meta` /
// `msg.meta` under the `pathRefs` key (see
// `internal/pathlinks.MetaKey`); the frontend reads it via
// `getPathRefsFromMeta` and hands it to `pathLinkExtension.ts`, which
// builds a marked inline extension that emits link tokens during the
// initial parse. That is the only path that produces clickable path
// anchors — agent prose can no longer linkify itself.
//
// `findPathRanges` below is the local matcher; it has exactly one
// consumer left: `toolCardPreview.ts` uses it for trusted-source
// leading-path detection in tool card headers (the path comes from the
// provider's tool-call args, not free-form agent prose, so no fs check
// is needed). Don't add new callers — anything rendering untrusted
// prose must use the Go-validated allowlist via `getPathRefsFromMeta`.
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

import type { PathRef } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

// Memo for `getPathRefsFromMeta`, keyed by the exact meta string.
// Identity stability is the point, not just parse cost: streaming rows
// are replaced on every reveal frame (`items[index] = {...}`), so their
// `$derived` pathRefs re-evaluate per frame while `item.meta` is the
// same string. Returning the SAME array for the same meta lets Svelte's
// derived equality stop propagation — otherwise a fresh array identity
// per frame rebuilds ChatMarkdown's marked extension, and a changed
// `streamdown.extensions` re-lexes every mounted markdown block per
// frame (Block.svelte's `tokens` derived). Bounded LRU; entries are
// small (a handful of validated paths per row).
const PATH_REFS_MEMO_CAP = 128;
const pathRefsMemo: Map<string, PathRef[] | undefined> = new Map();

/**
 * Read the Go-validated `pathRefs` allowlist out of an item's meta
 * JSON. Returns `undefined` when the meta has no `pathRefs` key (the
 * common case for pre-pathlinks history rows, and for non-assistant
 * kinds that don't get enriched). Returns a defensively-filtered
 * `PathRef[]` otherwise — entries with missing `path` strings drop
 * silently so a malformed meta can't crash the linkifier.
 *
 * Memoized on the meta string: repeated calls with the same meta return
 * the same array identity (see `pathRefsMemo` above).
 */
export function getPathRefsFromMeta(meta: string | undefined | null): PathRef[] | undefined {
  if (meta === undefined || meta === null || meta === '') return undefined;
  if (pathRefsMemo.has(meta)) {
    const memoized = pathRefsMemo.get(meta);
    // Refresh recency (Map iteration order is insertion order).
    pathRefsMemo.delete(meta);
    pathRefsMemo.set(meta, memoized);
    // Re-canonicalize on every hit, not just on parse: a draining
    // row's per-frame calls hit this branch, and without the refresh
    // its CONTENT entry would keep the recency stamp from the last
    // meta change — 128 distinct allowlists parsed elsewhere before
    // settle would evict it, and the settle meta would mint a fresh
    // identity (the re-lex regression, back for exactly the row that
    // needs the guarantee). If the entry was already evicted, this
    // reinstalls the live instance as canonical.
    return canonicalizePathRefs(memoized);
  }
  const out = canonicalizePathRefs(parsePathRefsFromMeta(meta));
  if (pathRefsMemo.size >= PATH_REFS_MEMO_CAP) {
    const oldest = pathRefsMemo.keys().next().value;
    if (oldest !== undefined) pathRefsMemo.delete(oldest);
  }
  pathRefsMemo.set(meta, out);
  return out;
}

// Content-canonical instances for the parsed arrays, keyed by the refs'
// serialized content. The meta-string memo above alone is not enough
// for identity stability: the settle patch merges the codeSpans blob
// into the SAME meta string the allowlist rides on, so a row's meta
// changes at settle while its pathRefs content is byte-identical — and
// a string-keyed miss returning a fresh array identity re-lexes every
// mounted markdown block mid-drain (ChatMarkdown extension rebuild;
// regression: ChatMarkdown.settleRelex.test.ts). Canonicalizing by
// content makes any meta that parses to the same allowlist resolve to
// ONE array instance, so Svelte's derived equality cuts the chain.
const pathRefsByContent: Map<string, PathRef[]> = new Map();

function canonicalizePathRefs(refs: PathRef[] | undefined): PathRef[] | undefined {
  if (!refs) return undefined;
  let key = '';
  for (const ref of refs) {
    // NUL/SOH separators: paths may contain any printable char, so a
    // printable-joined key could collide across field/ref boundaries.
    key += `${ref.path}\u0000${ref.line ?? ''}\u0000${ref.col ?? ''}\u0001`;
  }
  const canonical = pathRefsByContent.get(key);
  if (canonical) {
    // Refresh recency (Map iteration order is insertion order).
    pathRefsByContent.delete(key);
    pathRefsByContent.set(key, canonical);
    return canonical;
  }
  if (pathRefsByContent.size >= PATH_REFS_MEMO_CAP) {
    const oldest = pathRefsByContent.keys().next().value;
    if (oldest !== undefined) pathRefsByContent.delete(oldest);
  }
  pathRefsByContent.set(key, refs);
  return refs;
}

function parsePathRefsFromMeta(meta: string): PathRef[] | undefined {
  const parsed = parseJsonObject(meta);
  if (!parsed) return undefined;
  const raw = parsed.pathRefs;
  if (!Array.isArray(raw)) return undefined;
  const out: PathRef[] = [];
  for (const entry of raw) {
    if (!entry || typeof entry !== 'object') continue;
    const e = entry as Record<string, unknown>;
    if (typeof e.path !== 'string' || e.path === '') continue;
    const ref: PathRef = { path: e.path };
    if (typeof e.line === 'number' && e.line > 0) ref.line = e.line;
    if (typeof e.col === 'number' && e.col > 0) ref.col = e.col;
    out.push(ref);
  }
  return out.length > 0 ? out : undefined;
}

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
// and `@scope/pkg` (the `@` is the immediately-preceding char). The
// rejection of `@`-prefixed tokens is intentional here: this regex is
// the local matcher used by `toolCardPreview.ts` against
// trusted-source tool-call args; without fs validation, an
// `@scope/pkg.something` shape can't be told apart from
// `@workspace/file.ts`. Untrusted prose goes through the Go-validated
// allowlist on `item.meta.pathRefs` instead — that path widens to
// `@workspace/...` shapes safely because each entry has already been
// validated by `internal/pathlinks.ExtractAndValidate` against the
// workspace fs.
// The `-endLine` alternative on the suffix is non-capturing — the
// caller (`toolCardPreview.ts`) only opens at the start line, so the
// range bound is consumed (keeping the matched substring aligned with
// the full token) without being exposed.
//
// Keep the supported suffix shapes in lockstep with
// `pathLinkExtension.ts` — both files must accept the same `:line` /
// `:line:col` / `:line-endLine` variants so a future regex tweak to
// one surface doesn't silently leave the other behind.
const PATH_PATTERN =
  /(?:^|(?<=[\s(\[{,;'"`<>=]))((?:\.{0,2}\/|\/)?[\w.\-~]+(?:\/[\w.\-~]+)+)(?::(\d+)(?:-\d+|:(\d+))?)?/g;

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
  // Reject trailing-dot tokens like `something/else.` — common in
  // prose ("see something/else.") and never a real filename. This
  // mirrors the same rejection on the Go side
  // (`internal/pathlinks/pathlinks.go`).
  if (finalSegment.endsWith('.')) return false;
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
    // Groups: 1 = path body, 2 = line, 3 = col. The leading boundary
    // is non-capturing (zero-width lookbehind).
    const [, pathToken, lineRaw, colRaw] = match;
    if (!looksLikeFilePath(pathToken)) continue;
    // `match.index` points to the first character of the path body.
    // `match[0]` is the path body plus optional `:line:col` suffix.
    const matchStart = match.index;
    const matchEnd = match.index + match[0].length;
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
