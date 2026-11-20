// The fact keys a PR/MR body's repository files can read
// (forgeRepositoryFiles.ts), found from its raw text when its snapshot is
// applied, so an image or a permalink paints in its final shape from the
// first frame. A prefetch, not the authority: the scans use the table, the
// URL end and the decoding the parse uses and aim to name every key a lex
// reads (and may name more: text inside code), and a key they miss is
// asked for when a block reads it (stores/forgeFacts.svelte.ts), at the
// cost of that block changing once.

import { backpedalUrl, firstSrcsetCandidate, htmlAttributeValue, resolveLinkText } from '../markdown';
import { imageFactKey, linesFactKey, type FactRevision } from './forgeFacts';
import type { FactKeySink, ForgeShapes } from './forgeReferenceExtension';
import { permalinkOf, repositoryImageOf } from './forgeRepositoryFiles';

/** Observability for tests: characters the image scan walked past its first two searches. */
export const repositoryScanCounters = { steps: 0 };

/** A Markdown destination as the parse reads it (`resolveLinkText`); one with no `&` and no `\` is as written. */
const destinationText = (written: string): string =>
  written.includes('&') || written.includes('\\') ? resolveLinkText(written) : written;

/** A link label as references match it. A line's container prefix (`> `) is not part of it. */
const labelKey = (label: string): string => label.replace(/[\r\n]+[ \t>]*/g, ' ').trim().replace(/\s+/g, ' ').toLowerCase();

const isSpaceOrTab = (code: number): boolean => code === 32 || code === 9;

/** Past the spaces, tabs and at most one line break (with the next line's container prefix) at `from`, as the link rule's `\s*`. */
function pastLinkSpace(body: string, from: number): number {
  let index = from;
  while (isSpaceOrTab(body.charCodeAt(index))) index++;
  const code = body.charCodeAt(index);
  // The parse reads `\r\n` and `\r` as `\n`.
  if (code === 10 || code === 13) {
    index += code === 13 && body.charCodeAt(index + 1) === 10 ? 2 : 1;
    for (let code = body.charCodeAt(index); isSpaceOrTab(code) || code === 62 /* > */; code = body.charCodeAt(++index));
  }
  repositoryScanCounters.steps += index - from;
  return index;
}

/**
 * Where a plain destination that starts at `from` ends: at a space or
 * control character, or at a `)` it does not open, as the engine cuts its
 * href (`findClosingBracket`). Records in `walks.ends`, for each `(` the
 * walk passes, where a destination right after it ends (its closing `)`,
 * or where the walk stopped), so a destination nested in this one is not
 * walked again, and, for a walk a space or the text's end stopped, the
 * last `)` before that end.
 */
function plainDestinationEnd(body: string, from: number, walks: DestinationWalks): number {
  const open: number[] = [];
  let lastClose = -1;
  let index = from;
  for (; index < body.length; index++) {
    const code = body.charCodeAt(index);
    if (code <= 32) break;
    if (code === 92 /* \ */) {
      // A space ends the href even after a backslash.
      const next = body.charCodeAt(index + 1);
      if (next > 32) index++;
      if (next === 41) lastClose = index;
    } else if (code === 40 /* ( */) {
      open.push(index);
    } else if (code === 41 /* ) */) {
      if (open.length === 0) break;
      walks.ends.set(open.pop()!, index);
      lastClose = index;
    }
  }
  const end = Math.min(index, body.length);
  for (const at of open) walks.ends.set(at, end);
  if (body.charCodeAt(end) !== 41 && !walks.lastCloses.has(end)) walks.lastCloses.set(end, lastClose);
  repositoryScanCounters.steps += end - from;
  return end;
}

/** What the destination walks of one body learned (`plainDestinationEnd`). */
interface DestinationWalks {
  ends: Map<number, number>;
  lastCloses: Map<number, number>;
}

/** Whether the `>` at `at` is not escaped: an even run of backslashes before it. */
function unescaped(body: string, at: number): boolean {
  let slashes = 0;
  while (body.charCodeAt(at - 1 - slashes) === 92) slashes++;
  return slashes % 2 === 0;
}

/**
 * The destination of the inline link or image whose `(` is at `paren`, as
 * the engine reads its `link` rule, given to `add`. A `<…>` destination
 * holds escapes and no `<` or line break; one that is not is read plainly
 * and keeps its brackets only when it ends in an unescaped `>`. False once
 * `add` is full.
 */
function inlineDestination(body: string, paren: number, walks: DestinationWalks, add: (href: string) => boolean): boolean {
  const start = pastLinkSpace(body, paren + 1);
  if (body.charCodeAt(start) === 60 /* < */) {
    let index = start + 1;
    for (let code = body.charCodeAt(index); index < body.length; code = body.charCodeAt(++index)) {
      if (code === 92 && body.charCodeAt(index + 1) !== 10) index++;
      else if (code === 10 || code === 60 || code === 92) break;
      else if (code === 62 /* > */) {
        repositoryScanCounters.steps += index - start;
        return index === start + 1 || add(destinationText(body.slice(start + 1, index)));
      }
    }
    repositoryScanCounters.steps += index - start;
    // Read plainly: the href runs to the last `)` before a space the
    // rule's backtracking allows, so each `>` before a `)` or the run's
    // end may be the one it keeps.
    let end = start;
    while (end < body.length && body.charCodeAt(end) > 32) end++;
    repositoryScanCounters.steps += end - start;
    for (let gt = body.indexOf('>', start + 1); gt !== -1 && gt < end; gt = body.indexOf('>', gt + 1)) {
      if ((gt + 1 === end || body.charCodeAt(gt + 1) === 41) && unescaped(body, gt) && !add(destinationText(body.slice(start + 1, gt)))) return false;
    }
    return true;
  }
  // A destination right after the `(` is the one `ends` may know.
  const end = start === paren + 1 ? (walks.ends.get(paren) ?? plainDestinationEnd(body, start, walks)) : plainDestinationEnd(body, start, walks);
  if (start === paren + 1) walks.ends.set(paren, end);
  if (end === start) return true;
  if (!add(destinationText(body.slice(start, end)))) return false;
  if (body.charCodeAt(end) === 41 /* ) */) return true;
  // Ended by a space or the text's end: unless a `)` follows, the rule's
  // backtracking keeps the href before the run's last `)`.
  const close = walks.lastCloses.get(end) ?? -1;
  return close <= start || add(destinationText(body.slice(start, close)));
}

/** Where the reference label opening at `from` (after its `[`) closes, or -1: it holds no unescaped `[`. */
function referenceLabelEnd(body: string, from: number): number {
  let index = from;
  for (; index < body.length; index++) {
    const code = body.charCodeAt(index);
    if (code === 92) index++;
    else if (code === 91) break;
    else if (code === 93) {
      repositoryScanCounters.steps += index - from;
      return index;
    }
  }
  repositoryScanCounters.steps += index - from;
  return -1;
}

/** Whether the line after the line break at `at` is blank. */
function blankLineAfter(body: string, at: number): boolean {
  let index = at + 1;
  while (isSpaceOrTab(body.charCodeAt(index))) index++;
  repositoryScanCounters.steps += index - at - 1;
  return index >= body.length || body.charCodeAt(index) === 10 || body.charCodeAt(index) === 13;
}

interface Opener {
  /** Index after the `[`. */
  at: number;
  image: boolean;
}

/**
 * Each Markdown image's destination, inline or by a reference a definition
 * gives. Brackets pair as the engine's label rule allows them and more
 * (any depth), within a paragraph. A backtick in an image's label may open
 * a code span that holds a bracket (`_inlineLabel`), so a `]` after one
 * leaves the image open and a `[` after one does not hide its `](`. A
 * reference's label holds no bracket (`_blockLabel`), so the labels read
 * are disjoint runs of the text. False once `add` is full.
 */
function markdownImages(body: string, from: number, add: (href: string) => boolean): boolean {
  const opens: Opener[] = [];
  // The `at` of each image opener in `opens`, in order.
  const images: number[] = [];
  const walks: DestinationWalks = { ends: new Map(), lastCloses: new Map() };
  let tick = -1;
  let bracket = -1;
  let labels: Set<string> | null = null;
  for (let index = from; index < body.length; index++) {
    const code = body.charCodeAt(index);
    if (code === 92 /* \ */) {
      index++;
    } else if (code === 96 /* ` */) {
      tick = index;
    } else if (code === 10) {
      // A label never spans a paragraph.
      if (opens.length > 0 && blankLineAfter(body, index)) {
        opens.length = 0;
        images.length = 0;
      }
    } else if (code === 33 /* ! */ && body.charCodeAt(index + 1) === 91) {
      opens.push({ at: index + 2, image: true });
      images.push(index + 2);
      bracket = ++index;
    } else if (code === 91 /* [ */) {
      opens.push({ at: index + 1, image: false });
      bracket = index;
    } else if (code === 93 /* ] */) {
      const inner = bracket;
      bracket = index;
      const open = opens.pop();
      if (open === undefined) continue;
      if (open.image) images.pop();
      const spanned = tick > Math.min(open.image ? open.at : Infinity, images[0] ?? Infinity);
      if (!open.image && !spanned) continue;
      const next = body.charCodeAt(index + 1);
      if (next === 40 /* ( */) {
        if (!inlineDestination(body, index + 1, walks, add)) return false;
      } else {
        const ref = next === 91 ? referenceLabelEnd(body, index + 2) : -1;
        if (ref > index + 2) (labels ??= new Set()).add(labelKey(body.slice(index + 2, ref)));
        else if (open.image && inner < open.at) (labels ??= new Set()).add(labelKey(body.slice(open.at, index)));
      }
      if (open.image && tick > open.at) {
        opens.push(open);
        images.push(open.at);
      }
    }
  }
  repositoryScanCounters.steps += body.length - from;
  return labels === null || definitions(body, labels, add);
}

/** A line's container prefix: indentation, `>` markers and list markers. */
const CONTAINER_PREFIX = /(?:[ \t>]|[-*+][ \t]|\d{1,9}[.)][ \t])*/y;

/**
 * The destination of each reference definition whose label is in
 * `labels`, read as the engine's `def` rule reads it: a label that may
 * span lines and holds escapes but no unescaped bracket, then `]:`, the
 * destination on that line or the next, plain or in `<…>`. A `<…>`
 * destination ends at a `>` the rest of its line lets end it (spaces, then
 * the line's end or a title). False once `add` is full.
 */
function definitions(body: string, labels: Set<string>, add: (href: string) => boolean): boolean {
  for (let line = 0; line < body.length; ) {
    CONTAINER_PREFIX.lastIndex = line;
    CONTAINER_PREFIX.test(body);
    const open = CONTAINER_PREFIX.lastIndex;
    repositoryScanCounters.steps += open - line;
    if (body.charCodeAt(open) === 91 /* [ */) {
      const close = referenceLabelEnd(body, open + 1);
      if (close > open + 1 && body.charCodeAt(close + 1) === 58 /* : */ && labels.has(labelKey(body.slice(open + 1, close)))) {
        const start = pastLinkSpace(body, close + 2);
        if (body.charCodeAt(start) === 60 /* < */) {
          const lineEnd = body.indexOf('\n', start);
          const end = lineEnd === -1 ? body.length : lineEnd;
          repositoryScanCounters.steps += end - start;
          for (let gt = body.indexOf('>', start + 1); gt !== -1 && gt < end; gt = body.indexOf('>', gt + 1)) {
            let after = gt + 1;
            for (let code = body.charCodeAt(after); isSpaceOrTab(code) || code === 13; code = body.charCodeAt(++after));
            const next = body.charCodeAt(after);
            if ((after >= end || next === 34 || next === 39 || next === 40) && gt > start + 1 && !add(destinationText(body.slice(start + 1, gt)))) return false;
          }
        } else {
          let end = start;
          while (end < body.length && body.charCodeAt(end) > 32) end++;
          repositoryScanCounters.steps += end - start;
          if (end > start && !add(destinationText(body.slice(start, end)))) return false;
        }
      }
    }
    const lineEnd = body.indexOf('\n', open);
    if (lineEnd === -1) break;
    line = lineEnd + 1;
  }
  return true;
}

/** The start of a tag whose `src` (or a `<source>`'s `srcset`) the parse reads as an image's. */
const MEDIA_TAG = /<(?:img|video|(source))(?=[\t\n\f\r />])/gi;
/** A line inside a blockquote, up to its last marker before content. */
const QUOTED_LINE = /(?:[ \t]|[-*+][ \t]|\d{1,9}[.)][ \t])*>/y;

const isHtmlSpace = (code: number): boolean => code === 32 || code === 9 || code === 10 || code === 12 || code === 13;

/**
 * Each `src` of an `<img>`, `<video>` or `<source>` and each `<source>`
 * `srcset`'s first candidate (`firstSrcsetCandidate`), with its references
 * decoded as the HTML parser decodes them (`htmlAttributeValue`). False
 * once `add` is full.
 *
 * A tag is read as the HTML tokenizer reads its attributes: a quoted value
 * holds `>`, `<` and line breaks; names and unquoted values end where the
 * tokenizer ends them; `/` and a closing quote separate attributes. Where
 * the parse's HTML starts is a block boundary the scan does not know, so
 * every tag start is read, including one inside another tag's quoted
 * value. In a tag that starts on a blockquote line, the next lines'
 * `>` markers are the container's, as the parse strips them.
 *
 * The walk is linear in the body: a tokenizer stops at an attribute start
 * an earlier one read in the same state, since what follows reads the
 * same, so tokenizers out of step (one in a quoted or unquoted value where
 * another reads attributes) share all but the attribute they differ on,
 * and the tokenizers that start inside one unquoted value reuse its end.
 * `repositoryScanCounters.steps` counts the characters walked.
 */
function htmlImages(body: string, from: number, add: (href: string) => boolean): boolean {
  const length = body.length;
  const starts = new Set<number>();
  let unquotedFrom = -1;
  let unquotedEnd = -1;
  let lineEnd = -1;
  let quoted = false;

  const skip = (start: number): number => {
    let index = start;
    for (let code = body.charCodeAt(index); ; code = body.charCodeAt(index)) {
      if (code === 10 && quoted) {
        index++;
        for (code = body.charCodeAt(index); isSpaceOrTab(code) || code === 62; code = body.charCodeAt(++index));
      } else if (isHtmlSpace(code) || code === 47 /* / */) {
        index++;
      } else break;
    }
    repositoryScanCounters.steps += index - start;
    return index;
  };
  const closeOf = (open: number, code: number): number => {
    const close = body.indexOf(code === 34 ? '"' : "'", open + 1);
    repositoryScanCounters.steps += (close === -1 ? length : close) - open;
    return close;
  };
  const unquotedEndOf = (start: number): number => {
    if (start >= unquotedFrom && start <= unquotedEnd) return unquotedEnd;
    let index = start;
    for (let code = body.charCodeAt(index); index < length && !isHtmlSpace(code) && code !== 62; code = body.charCodeAt(++index));
    repositoryScanCounters.steps += index - start;
    unquotedFrom = start;
    unquotedEnd = index;
    return index;
  };
  const addValue = (written: string, srcset: boolean): boolean => {
    const value = htmlAttributeValue(written);
    const src = srcset ? firstSrcsetCandidate(value) : value;
    return !src || add(src);
  };

  MEDIA_TAG.lastIndex = from;
  for (let tag = MEDIA_TAG.exec(body); tag !== null; tag = MEDIA_TAG.exec(body)) {
    const at = tag.index;
    if (at > lineEnd) {
      const lineStart = body.lastIndexOf('\n', at) + 1;
      lineEnd = body.indexOf('\n', at);
      if (lineEnd === -1) lineEnd = length;
      repositoryScanCounters.steps += lineEnd - lineStart;
      QUOTED_LINE.lastIndex = lineStart;
      quoted = QUOTED_LINE.test(body) && QUOTED_LINE.lastIndex <= at;
    }
    const source = tag[1] !== undefined;
    // Attribute starts are keyed by the state that reads them: the
    // position, whether line markers are skipped, and the tag's kind.
    const state = (quoted ? 2 : 0) + (source ? 1 : 0);
    let index = at + tag[0].length;
    for (;;) {
      index = skip(index);
      if (index >= length || body.charCodeAt(index) === 62 /* > */) break;
      const key = index * 4 + state;
      if (starts.has(key)) break;
      starts.add(key);
      // The first character starts the name even when it is `=`.
      let nameEnd = index + 1;
      for (let code = body.charCodeAt(nameEnd); nameEnd < length && !isHtmlSpace(code) && code !== 47 && code !== 62 && code !== 61; code = body.charCodeAt(++nameEnd));
      repositoryScanCounters.steps += nameEnd - index;
      const nameLength = nameEnd - index;
      const name = nameLength === 3 || (source && nameLength === 6) ? body.slice(index, nameEnd).toLowerCase() : '';
      const wanted = name === 'src' || name === 'srcset';
      index = nameEnd;
      while (isHtmlSpace(body.charCodeAt(index))) index++;
      if (body.charCodeAt(index) !== 61 /* = */) continue;
      index++;
      while (isHtmlSpace(body.charCodeAt(index))) index++;
      const code = body.charCodeAt(index);
      if (code === 34 || code === 39) {
        const close = closeOf(index, code);
        // A tag the body ends in shows nothing.
        if (close === -1) break;
        if (wanted && !addValue(body.slice(index + 1, close), name === 'srcset')) return false;
        index = close + 1;
      } else if (index < length && code !== 62) {
        const valueEnd = unquotedEndOf(index);
        if (wanted && !addValue(body.slice(index, valueEnd), name === 'srcset')) return false;
        index = valueEnd;
      }
    }
  }
  return true;
}

/**
 * The `image:` keys of a body: every Markdown image (inline or by
 * reference) and HTML `<img>`, `<video>` or `<picture>` candidate whose
 * destination names a repository file, at the frame's commits. Nothing
 * runs past two searches for a body with no `![` and no `<`, and a
 * destination is decoded only when it holds a reference or an escape.
 * The walks are linear in the body (`repositoryScanCounters`); each
 * candidate read costs its own length besides, so candidates nested in
 * one another's destinations or unquoted values cost the sum of their
 * lengths.
 */
export function scanRepositoryImages(body: string, shapes: ForgeShapes, revision: FactRevision, add: FactKeySink): void {
  const image = body.indexOf('![');
  const tag = body.indexOf('<');
  if (image === -1 && tag === -1) return;
  const frame = () => revision;
  const addHref = (href: string): boolean => {
    const file = repositoryImageOf(href, shapes, frame);
    return file === null || add(imageFactKey(file.commit, file.path));
  };
  if (tag !== -1 && !htmlImages(body, tag, addHref)) return;
  if (image !== -1) markdownImages(body, image, addHref);
}

const BARE_URL = /https:\/\/[^\s<]+/g;

/**
 * The `lines:` keys of a body on GitHub: every bare URL that is a permalink
 * to lines of this repository, ended as the parse ends a bare URL. Nothing
 * runs past one search for a body with no `/blob/`.
 */
export function scanPermalinkLines(body: string, shapes: ForgeShapes, _revision: FactRevision, add: FactKeySink): void {
  if (!shapes.github || body.indexOf('/blob/') === -1) return;
  BARE_URL.lastIndex = 0;
  for (let match = BARE_URL.exec(body); match !== null; match = BARE_URL.exec(body)) {
    if (!match[0].includes('#L')) continue;
    const link = permalinkOf(backpedalUrl(match[0]), shapes);
    if (link && !add(linesFactKey(link.sha, link.path, link.start, link.end))) return;
  }
}
