// Marked inline extension that turns server-validated path tokens
// into clickable links DURING the initial markdown parse — replacing
// the legacy post-render DOM walker.
//
// The Go side (`internal/pathlinks`) is the only thing that decides
// whether a token is really a path on disk. This file ONLY consumes
// that allowlist; it never invents matches on its own. When the
// allowlist is empty, the extension returns `undefined` so callers can
// skip wiring it into Streamdown entirely.
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

function isBoundary(ch: string | undefined): boolean {
  return ch === undefined || BOUNDARY_CHARS.has(ch);
}

interface PathLinkExtension {
  name: 'pathLink';
  level: 'inline';
  start(src: string): number | undefined;
  tokenizer(this: unknown, src: string, tokens: Token[] | TokensList): GenericLinkToken | undefined;
}

// Shape that satisfies both marked's runtime contract and streamdown's
// `Extension` type without importing the latter (it lives behind the
// patched package's deep import path).
interface GenericLinkToken extends Tokens.Link {
  type: 'link';
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
 * paths. Returns `undefined` when the allowlist is empty so callers
 * can skip wiring it in entirely.
 *
 * `workspacePath` is encoded into each emitted href so a click after
 * the surface unmounts can still resolve relative paths — same trick
 * the legacy walker used with `data-workspace-path`.
 */
export function buildPathLinkExtension(
  pathRefs: readonly PathRef[],
  workspacePath: string,
): PathLinkExtension | undefined {
  if (pathRefs.length === 0) return undefined;

  // Dedupe by path (multiple refs for the same file may exist when
  // the same file is mentioned with different :line:col suffixes —
  // the suffix is extracted from the matched text, not the allowlist,
  // so a set of paths is the only data we need here).
  const allowed = new Set<string>();
  for (const ref of pathRefs) {
    if (ref?.path) allowed.add(ref.path);
  }
  if (allowed.size === 0) return undefined;

  // Longest-first ordering ensures `src/lib/foo.ts` matches before the
  // nested `foo.ts` when both happen to be in the allowlist.
  const paths = Array.from(allowed).sort((a, b) => b.length - a.length);

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
  const alternation = paths.map(escapeRegex).join('|');
  // Keep the supported suffix shapes in lockstep with
  // `pathLinkify.ts#PATH_PATTERN` — both files must accept the same
  // `:line` / `:line:col` / `:line-endLine` variants so an agent
  // referencing a range gets the same treatment from the prose
  // tokenizer and the tool-card preview matcher.
  const bareRe = new RegExp(`^(@)?(${alternation})${OPTIONAL_PATH_SUFFIX_SOURCE}`);
  const wrappedRe = new RegExp(`^\`(@)?(${alternation})${OPTIONAL_PATH_SUFFIX_SOURCE}\``);
  // Unanchored scanner for `start` — the whole allowlist as ONE pass. See
  // `earliestPathLinkHit`. Its `lastIndex` is reset on entry there, never
  // carried between calls.
  const scanRe = new RegExp(`(?:${alternation})`, 'g');

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
    title: null,
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

      if (src.startsWith('[')) {
        return markdownPathLinkToken(src, this, parseAllowlistedTarget, workspacePath);
      }

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
function earliestPathLinkHit(src: string, scanRe: RegExp): number | undefined {
  let earliest = -1;
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
  // A markdown link's hit is its label's `[`, which sits BEFORE the `](`
  // that identifies it — so it can beat a path hit that the scan above
  // already found, and cannot be folded into that scan.
  //
  // The `firstBracket` guard is a filter, not a speedup in general: a
  // markdown-link hit can never precede the source's first `[`, so a
  // source with no `[` at all skips the `](` search outright (the win —
  // most agent prose in a streaming tail has no brackets), while a source
  // that DOES contain one has paid an extra `indexOf` on top of the search
  // it was going to run anyway (a wash). The second clause is the part
  // that matters for bracket-heavy sources: when the scan already found a
  // hit earlier than the first `[`, no label can beat it, so the `](`
  // search is skipped even though brackets exist.
  const firstBracket = src.indexOf('[');
  if (firstBracket !== -1 && (earliest === -1 || firstBracket < earliest)) {
    const markdownHit = earliestMarkdownLinkHit(src);
    if (markdownHit !== undefined && (earliest === -1 || markdownHit < earliest)) {
      earliest = markdownHit;
    }
  }
  return earliest === -1 ? undefined : earliest;
}

function earliestMarkdownLinkHit(src: string): number | undefined {
  const idx = src.indexOf('](');
  if (idx <= 0) return undefined;
  const open = src.lastIndexOf('[', idx);
  return open === -1 ? undefined : open;
}

function markdownPathLinkToken(
  src: string,
  ctx: unknown,
  parseAllowlistedTarget: (target: string) => ParsedPathTarget | null,
  workspacePath: string,
): GenericLinkToken | undefined {
  const tokenizerContext = ctx as PathLinkTokenizerContext;
  const token = tokenizerContext.lexer?.tokenizer?.link?.(src);
  if (!token || token.type !== 'link') return undefined;
  const parsed = parseAllowlistedTarget(token.href);
  if (!parsed) return token;
  return {
    ...token,
    href: buildPathLinkHref(parsed.path, parsed.line, parsed.col, workspacePath),
  };
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
