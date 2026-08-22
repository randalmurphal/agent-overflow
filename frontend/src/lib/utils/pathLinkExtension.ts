// Marked inline extension that turns server-validated path tokens
// into clickable links DURING the initial markdown parse — replacing
// the legacy post-render DOM walker — and rewrites path-shaped
// markdown-link hrefs to the same editor scheme.
//
// For PROSE tokens, the Go side (`internal/pathlinks`) is the only
// thing that decides whether a token is really a path on disk. This
// file never invents prose matches on its own. Markdown-link HREFS
// (`[label](/abs/file.md)`) are the deliberate exception: they are
// explicit link destinations the user must click, so they become
// editor affordances without render-time validation and the backend
// (`editor.ResolvePath`) is the gate at click time — see
// `parsePathShapedHref`.
//
// Token shape: emits a marked `link` token whose `href` is
// `agent-overflow:open?path=…&line=…&col=…&workspace=…`. Streamdown's
// built-in Link element renders this as a plain `<a href=…>` once the
// parent passes `agent-overflow:` through `allowedLinkPrefixes`. A
// document-level click delegate in `markdownEnhance.ts` intercepts
// clicks on those anchors and forwards to the `OpenInEditor` binding.
//
// Why marked + token-time, not a post-render walker:
//   - The walker ran after `streaming` flipped false, replacing text
//     nodes with anchors — the visible "shift" at the end of a stream
//     that motivated this rewrite.
//   - Token-time means the link is part of the very first paint of
//     each streaming chunk: as the path completes and Go validates it,
//     the next emit re-parses and the link IS already there.
//   - Marked's inline tokenizer is invoked between built-in tokens, so
//     code spans (`` `src/foo.ts` ``) and fenced code blocks are
//     correctly skipped without any DOM ancestry tests.

import type { Token, Tokens, TokensList } from 'marked';
import type { PathRef } from '../types/models';
import { openInEditorLabel } from './editorLinkLabel';

// Per-page-load nonce that gates our `agent-overflow:open?…` scheme.
// Streamdown's `transformUrl` honors a custom-scheme prefix only when
// the URL `startsWith(prefix.href)` (see svelte-streamdown/dist/utils/
// url.js). By baking the nonce into the prefix we hand to Streamdown,
// raw agent prose like `[click](agent-overflow:open?path=/etc/passwd)`
// is rejected at the URL filter — the agent's input is markdown text
// and can never observe the rendered nonce, so it cannot forge a
// passing prefix. Our extension constructs hrefs starting with the
// same nonce-prefixed form, so legitimate links round-trip.
//
// Crypto: 16 bytes (128 bits) is more than enough — the nonce only
// needs to be unpredictable to a single page-load's worth of agent
// turns. `crypto.getRandomValues` is available in every modern
// browser + happy-dom + Node 18+.
const PATH_LINK_NONCE = generatePathLinkNonce();

function generatePathLinkNonce(): string {
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes);
  } else {
    // SSR / test environments without webcrypto — fail closed by
    // generating a session-stable value (Math.random is good enough
    // here because there's no live browser to attack).
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  }
  let hex = '';
  for (let i = 0; i < bytes.length; i += 1) hex += bytes[i].toString(16).padStart(2, '0');
  return hex;
}

// Public href prefix — exported so the click delegate, copy
// serializer, and ChatMarkdown's `allowedLinkPrefixes` can detect our
// links by href, not by class. Includes the nonce so a raw markdown
// link written by an agent cannot satisfy this prefix.
export const PATH_LINK_HREF_PREFIX = `agent-overflow:open?nonce=${PATH_LINK_NONCE}&`;
export const LOCAL_IMAGE_HREF_PREFIX = `agent-overflow:image?nonce=${PATH_LINK_NONCE}&`;

// Boundary chars that may legitimately precede a path token. Mirrors
// the lookbehind set used by `pathLinkify.ts` (the legacy DOM walker)
// so behaviour stays the same across the rewrite. Email-shaped
// patterns like `name@host/path.ts` are rejected because `e` (the
// alphanumeric char before `@`) is not in this set.
const BOUNDARY_CHARS = new Set<string>([
  ' ', '\t', '\n', '\r',
  '(', '[', '{',
  ',', ';', "'", '"', '`',
  '<', '>', '=',
]);

// Shared suffix grammar for every path-link surface:
// `:line`, `:line:col`, and `:line-endLine`. The editor opens at the
// start line for ranges, so the end line is intentionally consumed but
// not captured.
const PATH_SUFFIX_SOURCE = `:(\\d+)(?:-\\d+|:(\\d+))?`;
const OPTIONAL_PATH_SUFFIX_SOURCE = `(?:${PATH_SUFFIX_SOURCE})?`;
const PATH_SUFFIX_RE = new RegExp(`^${PATH_SUFFIX_SOURCE}$`);
// Greedy TRAILING RUN of colon-digit groups, validated as a whole by
// parsePathSuffix afterwards. Matching just one suffix at the end would
// split `a.md:1:2:3` as path `a.md:1` + suffix `:2:3`; matching the run
// and letting the anchored validator reject it keeps the whole string
// as the path instead.
const PATH_SUFFIX_RUN_AT_END_RE = /(?::\d+(?:-\d+)?)+$/;
// URI scheme shape (RFC 3986). Hoisted so it isn't re-allocated per
// markdown link per lex pass.
const SCHEME_RE = /^[a-zA-Z][a-zA-Z0-9+.-]*:/;

function isBoundary(ch: string | undefined): boolean {
  return ch === undefined || BOUNDARY_CHARS.has(ch);
}

interface PathLinkExtension {
  name: 'pathLink';
  level: 'inline';
  start(src: string): number | undefined;
  tokenizer(
    this: unknown,
    src: string,
    tokens: Token[] | TokensList,
  ): GenericLinkToken | GenericImageToken | undefined;
}

// Shape that satisfies both marked's runtime contract and streamdown's
// `Extension` type without importing the latter (it lives behind the
// patched package's deep import path).
interface GenericLinkToken extends Tokens.Link {
  type: 'link';
}

interface GenericImageToken extends Tokens.Image {
  type: 'image';
}

interface PathLinkTokenizerContext {
  lexer?: {
    state?: {
      inLink?: boolean;
    };
    tokenizer?: {
      link?: (src: string) => Tokens.Link | Tokens.Image | undefined;
    };
  };
}

interface ParsedPathTarget {
  path: string;
  line: number | undefined;
  col: number | undefined;
}

/**
 * Build a marked inline extension that linkifies the allowlisted
 * paths in PROSE, and rewrites path-shaped markdown-link HREFS
 * (`[label](/abs/file.md)`, `[x](~/notes.md)`, `[x](docs/foo.md)`)
 * to the same nonce'd editor scheme.
 *
 * The two halves have different trust models, on purpose:
 *   - Prose linkification only ever consumes the server-validated
 *     allowlist — this file never invents prose matches on its own.
 *   - Markdown-link hrefs are agent- or third-party-authored
 *     destinations the user must CLICK; the rewrite makes them an
 *     editor affordance and the backend (`editor.ResolvePath`)
 *     enforces openability at click time — an existing regular file
 *     anywhere, new files only inside the workspace, folder opens
 *     never. A refused click surfaces as a toast. Without the rewrite
 *     these hrefs are worse than useless: a raw `/`-leading href is a
 *     same-tab navigation onto the SPA origin (a 404), so the
 *     vendored streamdown Link element refuses to render raw anchors
 *     for them at all.
 *
 * Href rewriting requires a non-empty `workspacePath` for EVERY shape
 * (see parsePathShapedHref); a surface with no workspace gets href
 * rewriting disabled entirely, so PR bodies / review comments —
 * third-party text full of `/owner/repo/...` root-relative links —
 * never grow editor affordances.
 *
 * Returns undefined when BOTH halves would be inert (empty allowlist
 * and no workspace): callers hand marked no extension at all then,
 * which keeps streamdown's extension-identity lex cache on its fast
 * path for those surfaces.
 *
 * `workspacePath` is encoded into each emitted href so a click after
 * the surface unmounts can still resolve relative paths — same trick
 * the legacy walker used with `data-workspace-path`.
 */
export function buildPathLinkExtension(
  pathRefs: readonly PathRef[],
  workspacePath: string,
): PathLinkExtension | undefined {
  // Dedupe by path (multiple refs for the same file may exist when
  // the same file is mentioned with different :line:col suffixes —
  // the suffix is extracted from the matched text, not the allowlist,
  // so a set of paths is the only data we need here).
  const allowed = new Set<string>();
  for (const ref of pathRefs) {
    if (ref?.path) allowed.add(ref.path);
  }

  // Longest-first ordering ensures `src/lib/foo.ts` matches before the
  // nested `foo.ts` when both happen to be in the allowlist.
  const paths = Array.from(allowed).sort((a, b) => b.length - a.length);

  if (paths.length === 0 && workspacePath === '') {
    // No prose allowlist and no workspace to anchor href rewriting:
    // the extension would match nothing. Hand marked nothing instead.
    return undefined;
  }

  // Two anchored regexes used by the tokenizer:
  //   bareRe    — `^(@)?<path>(?::\d+(?:-\d+|:\d+)?)?`
  //   wrappedRe — ``^`(@)?<path>(?::\d+(?:-\d+|:\d+)?)?` ``  (backtick-wrapped)
  // Group 1 = optional `@` prefix; group 2 = matched path; group 3 =
  // line; group 4 = col when the `:col` form is used. The `-endLine`
  // alternative is non-capturing — clicking still opens at the start
  // line, so the range bound is consumed (keeping the wrapped regex
  // anchored to the trailing backtick) but not forwarded to the
  // editor.
  //
  // Wrapped form wins against marked's built-in codespan tokenizer
  // because inline extensions run BEFORE built-ins at every position
  // (see marked@18 Lexer.inlineTokens). The emitted link token carries
  // a `codespan` child so Streamdown's Block + Element render
  // `<a><code>…</code></a>` — keeping the monospace pill UX users
  // expect for paths in prose while still routing the click to the
  // OpenInEditor binding.
  // All three prose regexes are null when the allowlist is empty: an
  // empty alternation matches the empty string at every position,
  // which would send the tokenizer into a zero-width loop. The
  // markdown-link branch below needs none of them.
  const alternation = paths.length > 0 ? paths.map(escapeRegex).join('|') : null;
  // Keep the supported suffix shapes in lockstep with
  // `pathLinkify.ts#PATH_PATTERN` — both files must accept the same
  // `:line` / `:line:col` / `:line-endLine` variants so an agent
  // referencing a range gets the same treatment from the prose
  // tokenizer and the tool-card preview matcher.
  const bareRe = alternation
    ? new RegExp(`^(@)?(${alternation})${OPTIONAL_PATH_SUFFIX_SOURCE}`)
    : null;
  const wrappedRe = alternation
    ? new RegExp(`^\`(@)?(${alternation})${OPTIONAL_PATH_SUFFIX_SOURCE}\``)
    : null;
  // Unanchored scanner for `start` — the whole allowlist as ONE pass. See
  // `earliestPathLinkHit`. Its `lastIndex` is reset on entry there, never
  // carried between calls.
  const scanRe = alternation ? new RegExp(`(?:${alternation})`, 'g') : null;

  // Build a link token from a successful regex match. `childKind`
  // selects the marked token type that wraps the visible text inside
  // the anchor — `'codespan'` for the wrapped form (renders `<code>`)
  // or `'text'` for the bare form. Centralizing the shape keeps the
  // two tokenizer branches lockstep on href/line/col semantics.
  const buildLinkToken = (
    raw: string,
    path: string,
    line: number | undefined,
    col: number | undefined,
    childKind: 'codespan' | 'text',
    inner: string,
  ): GenericLinkToken => ({
    type: 'link',
    raw,
    href: buildPathLinkHref(path, line, col, workspacePath),
    title: openInEditorLabel(path, line, col),
    text: inner,
    tokens: [{ type: childKind, raw, text: inner }],
  });

  const parseAllowlistedTarget = (target: string): ParsedPathTarget | null => {
    for (const path of paths) {
      if (!target.startsWith(path)) continue;
      const suffixText = target.slice(path.length);
      const suffix = parsePathSuffix(suffixText);
      if (!suffix) continue;
      return { path, line: suffix.line, col: suffix.col };
    }
    return null;
  };

  return {
    name: 'pathLink',
    level: 'inline',
    start(src) {
      return earliestPathLinkHit(src, scanRe);
    },
    tokenizer(this: unknown, src, tokens) {
      if (isInsideMarkdownLinkLabel(this)) return undefined;

      if (src.startsWith('[') || src.startsWith('![')) {
        return markdownLocalResourceToken(src, this, parseAllowlistedTarget, workspacePath);
      }
      if (!bareRe || !wrappedRe) return undefined;

      // Boundary check: marked invokes inline extensions at every
      // position it advances to — including positions reached by
      // consuming non-extension text. Without this guard, an
      // email-shaped pattern like `name@src/foo.ts` would match once
      // marked emits `name` as text and lands on `@src/foo.ts`. Both
      // branches require this guard — for the wrapped branch it
      // rejects `x\`src/foo.ts\``-glued-to-alphanumeric shapes too.
      const prev = lastConsumedChar(tokens);
      if (!isBoundary(prev)) return undefined;

      // Try wrapped form first when src begins with a backtick. If it
      // fails (path not in allowlist, or unbalanced backticks), return
      // undefined so marked falls through to its built-in codespan
      // tokenizer and renders the unrelated code span normally.
      if (src.startsWith('`')) {
        const w = wrappedRe.exec(src);
        if (!w) return undefined;
        const path = w[2];
        if (!allowed.has(path)) return undefined;
        const raw = w[0];
        // Strip the surrounding backticks for the inner codespan text;
        // paths never start or end with spaces (the Go-side regex in
        // pathlinks rejects them), so marked's built-in single-space
        // trim is intentionally not replicated here.
        const inner = raw.slice(1, -1);
        const line = w[3] ? Number(w[3]) : undefined;
        const col = w[4] ? Number(w[4]) : undefined;
        return buildLinkToken(raw, path, line, col, 'codespan', inner);
      }

      const m = bareRe.exec(src);
      if (!m) return undefined;
      const path = m[2];
      if (!allowed.has(path)) return undefined;
      const raw = m[0];
      const line = m[3] ? Number(m[3]) : undefined;
      const col = m[4] ? Number(m[4]) : undefined;
      // Bare branch's visible text IS the full match (including any
      // captured `:line:col`), so the inner text matches `raw`.
      return buildLinkToken(raw, path, line, col, 'text', raw);
    },
  };
}

/**
 * Compose the custom-scheme href the click delegate parses. Exported
 * so tests can assert exact shape without re-implementing the format.
 */
export function buildPathLinkHref(
  path: string,
  line: number | undefined,
  col: number | undefined,
  workspacePath: string,
): string {
  // The nonce is already baked into PATH_LINK_HREF_PREFIX (which ends
  // in `&`), so the remaining params start cleanly.
  const params = new URLSearchParams();
  params.set('path', path);
  if (line && line > 0) params.set('line', String(line));
  if (col && col > 0) params.set('col', String(col));
  if (workspacePath) params.set('workspace', workspacePath);
  return `${PATH_LINK_HREF_PREFIX}${params.toString()}`;
}

/**
 * Parse the href back into the click-delegate's argument shape.
 * Returns null when the href doesn't belong to us.
 */
export function parsePathLinkHref(href: string | null | undefined): {
  path: string;
  line: number;
  col: number;
  workspacePath: string;
} | null {
  if (!href || !href.startsWith(PATH_LINK_HREF_PREFIX)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  const path = url.searchParams.get('path');
  if (!path) return null;
  const line = Number(url.searchParams.get('line') ?? '0') || 0;
  const col = Number(url.searchParams.get('col') ?? '0') || 0;
  const workspacePath = url.searchParams.get('workspace') ?? '';
  return { path, line, col, workspacePath };
}

export function buildLocalImageHref(
  path: string,
  workspacePath: string,
  sourceHref = '',
): string {
  const params = new URLSearchParams();
  params.set('path', path);
  params.set('workspace', workspacePath);
  if (sourceHref) params.set('source', sourceHref);
  return `${LOCAL_IMAGE_HREF_PREFIX}${params.toString()}`;
}

export function parseLocalImageHref(href: string | null | undefined): {
  path: string;
  workspacePath: string;
  sourceHref: string;
} | null {
  if (!href || !href.startsWith(LOCAL_IMAGE_HREF_PREFIX)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  const path = url.searchParams.get('path');
  const workspacePath = url.searchParams.get('workspace');
  if (!path || !workspacePath) return null;
  return { path, workspacePath, sourceHref: url.searchParams.get('source') ?? '' };
}

/**
 * Earliest position in `src` where an allowlisted path begins AND the
 * preceding character is a boundary (or position 0). Includes an
 * optional `@` prefix when the char before `@` is also a boundary, so
 * `@src/foo.ts` widens to include the `@`. Backtick is in
 * BOUNDARY_CHARS, so `` `src/foo.ts` `` also returns a hit at the
 * path body — the tokenizer's wrapped-form branch then matches the
 * full `` `…` `` span at the preceding backtick once marked stops
 * inlineText at the backtick (its built-in text regex does so
 * unconditionally).
 *
 * Marked's lexer consumes `[0, hit)` as a `text` token and then calls
 * `tokenizer` on `src.slice(hit)`. A return of `undefined` means
 * "no allowlisted path appears anywhere in this slice" and the lexer
 * falls through to its built-in tokenizers.
 *
 * Cost is the reason this takes a compiled scanner rather than the path
 * list. Marked calls every inline extension's `start` on every tokenizer
 * loop iteration, handing it a fresh `src.slice(…)` of whatever is left —
 * so this runs once per inline token per block, and a streaming tail
 * re-pays the whole thing on every reveal tick. Searching per path
 * (`src.indexOf(path)` for each) made that O(tokens × paths × |src|):
 * every allowlisted path that does NOT occur costs a full scan of the
 * source before the answer is known, and most of them never occur.
 *
 * One alternation over the whole allowlist collapses those into a single
 * left-to-right pass that STOPS at the first boundary-qualified hit. Be
 * precise about what that buys: it is NOT an allowlist-independent bound.
 * Leftmost alternation still considers each alternative at each position,
 * so a source with no match is O(|src| × N) in the worst case, same order
 * as the per-path loop. What changes is that the source is walked ONCE
 * instead of N times (so the engine's own first-character dispatch prunes
 * most alternatives per position, and the cache stays warm), and that a
 * document WITH a match stops at the first one instead of paying N-1 full
 * scans for the paths that never occur — which is the case that actually
 * dominates, because `start` is re-run over the whole remaining tail on
 * every inline token. Measured effect: parse time went from scaling ~10×
 * between a 1-path and a 64-path allowlist to flat (see the performance
 * contract in `pathLinkExtension.test.ts`).
 *
 * Non-qualifying hits restart the scan one character past the match (not
 * past the whole match) because a shorter allowlisted path may still begin
 * inside it — the same overlap rule the per-path loop had.
 */
function earliestPathLinkHit(src: string, scanRe: RegExp | null): number | undefined {
  let earliest = -1;
  if (scanRe) {
    scanRe.lastIndex = 0;
    for (let match = scanRe.exec(src); match !== null; match = scanRe.exec(src)) {
      const idx = match.index;
      const prev = idx === 0 ? undefined : src[idx - 1];
      if (isBoundary(prev)) {
        earliest = idx;
        break; // Scan order is document order — nothing earlier can follow.
      }
      if (prev === '@') {
        const prev2 = idx >= 2 ? src[idx - 2] : undefined;
        if (isBoundary(prev2)) {
          earliest = idx - 1;
          break;
        }
      }
      scanRe.lastIndex = idx + 1;
    }
  }
  // No bracket handling here on purpose: `start` only bounds how far
  // marked's inlineText run may consume, and marked's own text rules
  // (both `normal` and `gfm`) already stop unconditionally at `[` — so
  // the tokenizer is invoked at every bracket position regardless, and
  // scanning for `[`/`](` in `start` was pure cost. (The one shape a
  // bracket-start could not rescue either: a `[` glued inside an
  // autolinked URL is swallowed by marked's `url` tokenizer before
  // extension starts are consulted.)
  return earliest === -1 ? undefined : earliest;
}

function markdownLocalResourceToken(
  src: string,
  ctx: unknown,
  parseAllowlistedTarget: (target: string) => ParsedPathTarget | null,
  workspacePath: string,
): GenericLinkToken | GenericImageToken | undefined {
  const tokenizerContext = ctx as PathLinkTokenizerContext;
  const token = tokenizerContext.lexer?.tokenizer?.link?.(src);
  if (!token || (token.type !== 'link' && token.type !== 'image')) return undefined;
  const fileTarget = parseLocalFileHref(token.href, workspacePath);
  if (token.type === 'image') {
    if (!fileTarget) return token as GenericImageToken;
    return {
      ...token,
      href: buildLocalImageHref(fileTarget.path, workspacePath, token.href),
    } as GenericImageToken;
  }
  const parsed = fileTarget ?? parseAllowlistedTarget(token.href) ?? parsePathShapedHref(token.href, workspacePath);
  if (!parsed) return token;
  return {
    ...token,
    href: buildPathLinkHref(parsed.path, parsed.line, parsed.col, workspacePath),
    title: openInEditorLabel(parsed.path, parsed.line, parsed.col),
  };
}

function splitPathSuffix(target: string): ParsedPathTarget {
  let path = target;
  let line: number | undefined;
  let col: number | undefined;
  const suffixMatch = PATH_SUFFIX_RUN_AT_END_RE.exec(target);
  if (suffixMatch && suffixMatch.index > 0) {
    const suffix = parsePathSuffix(target.slice(suffixMatch.index));
    if (suffix) {
      path = target.slice(0, suffixMatch.index);
      line = suffix.line;
      col = suffix.col;
    }
  }
  return { path, line, col };
}

function parseLocalFileHref(href: unknown, workspacePath: string): ParsedPathTarget | null {
  if (typeof href !== 'string' || workspacePath === '' || !/^file:/i.test(href)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  if (url.protocol !== 'file:') return null;
  const hostname = url.hostname.toLowerCase();
  if (hostname !== '' && hostname !== 'localhost') return null;

  let path = decodePathComponent(url.pathname);
  // WHATWG file URLs spell a Windows drive as `/C:/…`. The backend
  // needs the native `C:/…` path rather than a URL-rooted POSIX shape.
  if (/^\/[a-zA-Z]:\//.test(path)) path = path.slice(1);
  if (path === '') return null;

  const parsed = splitPathSuffix(path);
  const fragment = decodePathComponent(url.hash.slice(1));
  const location = /^L(\d+)(?:C(\d+)|-L?\d+)?$/.exec(fragment);
  if (location && parsed.line === undefined) {
    parsed.line = Number(location[1]);
    parsed.col = location[2] ? Number(location[2]) : undefined;
  }
  return parsed;
}

/**
 * Decide whether a markdown link's href is a local filesystem path and
 * split off an optional `:line[:col]` suffix. Returns null for
 * anything that is not path-shaped:
 *   - EVERY shape when no workspace is available. This is a security
 *     boundary, not just a UX call: surfaces without a workspace are
 *     the third-party ones (PR bodies, review comments, full of
 *     GitHub root-relative `/owner/repo/...` links), and a
 *     workspace-less href would reach `editor.ResolvePath`'s
 *     stat-free project-open pass-through. Requiring a workspace
 *     guarantees every click lands on the stat-gated branch.
 *   - hrefs with a scheme (`https:`, `mailto:`, forged
 *     `agent-overflow:` — the nonce check rejects those downstream)
 *   - network-path references (`//host/x`) and UNC shapes (`\\host\x`)
 *   - in-page fragments (`#…`) and bare queries (`?…`)
 *
 * A trailing `#fragment` / `?query` is stripped (`[x](docs/a.md#install)`
 * opens `docs/a.md`), and the path is percent-decoded — the standard
 * markdown spelling of a path with a space is `/a%20b.md`.
 *
 * Everything path-shaped becomes an editor affordance. Existence is
 * deliberately NOT checked here — the backend stats at click time and
 * a refused open surfaces as a toast, so a hallucinated path costs one
 * click, not a render-time stat per link.
 */
function parsePathShapedHref(href: unknown, workspacePath: string): ParsedPathTarget | null {
  if (typeof href !== 'string' || href.length === 0) return null;
  if (workspacePath === '') return null;
  // Any leading backslash is refused, not just literal `\\`: markdown
  // escaping halves backslashes before the href reaches us, so a UNC
  // source `[x](\\host\share)` arrives as `\host\share`.
  if (href.startsWith('//') || href.startsWith('\\')) return null;
  if (href.startsWith('#') || href.startsWith('?')) return null;
  const cut = href.search(/[#?]/);
  const target = cut === -1 ? href : href.slice(0, cut);
  if (target === '') return null;

  // Suffix split runs BEFORE the scheme check so `[x](Makefile:12)`
  // reads as path + line rather than as a `Makefile:` URI scheme. Port
  // shapes still land right: `http://host:8080` strips `:8080`, then
  // the remainder fails the scheme check anyway.
  const parsed = splitPathSuffix(target);
  const { path } = parsed;
  if (SCHEME_RE.test(path)) return null;
  parsed.path = decodePathComponent(path);
  return parsed;
}

// Percent-decode a path-shaped href component; a malformed escape
// (`%GZ`, lone `%`) falls through as the literal text rather than
// throwing away the link.
function decodePathComponent(raw: string): string {
  if (!raw.includes('%')) return raw;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function parsePathSuffix(suffix: string): {
  line: number | undefined;
  col: number | undefined;
} | null {
  if (suffix === '') {
    return { line: undefined, col: undefined };
  }
  const match = PATH_SUFFIX_RE.exec(suffix);
  if (!match) return null;
  const line = Number(match[1]);
  const col = match[2] ? Number(match[2]) : undefined;
  if (!Number.isSafeInteger(line) || line <= 0) return null;
  if (col !== undefined && (!Number.isSafeInteger(col) || col <= 0)) return null;
  return { line, col };
}

function isInsideMarkdownLinkLabel(ctx: unknown): boolean {
  const tokenizerContext = ctx as PathLinkTokenizerContext;
  return tokenizerContext.lexer?.state?.inLink === true;
}

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Last consumed character — the char immediately preceding the
 * tokenizer's current src position. Marked tokenizes incrementally,
 * so the prior token's text/raw is the only source of truth for
 * "what came before this match".
 */
function lastConsumedChar(tokens: Token[] | TokensList | undefined): string | undefined {
  if (!tokens || tokens.length === 0) return undefined;
  const last = tokens[tokens.length - 1] as { text?: string; raw?: string };
  if (typeof last.text === 'string' && last.text.length > 0) {
    return last.text[last.text.length - 1];
  }
  if (typeof last.raw === 'string' && last.raw.length > 0) {
    return last.raw[last.raw.length - 1];
  }
  return undefined;
}
