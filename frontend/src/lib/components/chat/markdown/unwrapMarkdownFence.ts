import {
  createProvenAppend,
  matchesProvenAppend,
  type ProvenAppend,
} from 'svelte-streamdown';

const OPENER_RE = /^ {0,3}(`{3,})(markdown|md)\s*$/i;
// Sticky twin of OPENER_RE for the fast path below: anchored at the
// candidate line's start via lastIndex instead of `^`, with `[^\S\n]*`
// standing in for the line-local `\s*$` (same class once '\n' — the
// line terminator `split` would have consumed — is excluded).
//
// MUST accept/reject exactly the lines OPENER_RE does — a divergence
// silently disables (or falsely triggers) the unwrap. Change the two
// together; the "fast-path equivalence" battery in
// unwrapMarkdownFence.test.ts pins the correspondence.
const OPENER_SCAN_RE = / {0,3}`{3,}(?:markdown|md)[^\S\n]*(?=\n|$)/iy;

function openerCanStillMatch(source: string): boolean {
  let index = 0;
  while (index < source.length && /\s/.test(source[index])) {
    index++;
    if (index > 3) return false;
  }
  if (index === source.length) return true;
  if (source[index] !== '`') return false;

  let ticks = 0;
  while (source[index + ticks] === '`') ticks++;
  if (index + ticks === source.length) return true;
  if (ticks < 3) return false;

  const languageStart = index + ticks;
  let languageEnd = languageStart;
  while (
    languageEnd < source.length &&
    /[A-Za-z]/.test(source[languageEnd])
  ) languageEnd++;
  const language = source.slice(languageStart, languageEnd).toLowerCase();
  if (!'markdown'.startsWith(language) && !'md'.startsWith(language)) return false;
  if (language !== 'markdown' && language !== 'md') {
    return languageEnd === source.length;
  }
  for (let i = languageEnd; i < source.length; i++) {
    if (source[i] === '\n') return true;
    if (!/[^\S\n]/.test(source[i])) return false;
  }
  return true;
}

interface MarkdownWrapperOpener {
  bodyStart: number;
  fenceLength: number;
}

function markdownWrapperOpener(source: string): MarkdownWrapperOpener | null {
  const firstBacktick = source.indexOf('`');
  if (firstBacktick === -1 || firstBacktick > 3) return null;
  let firstNonWs = 0;
  while (firstNonWs < firstBacktick && /\s/.test(source[firstNonWs])) firstNonWs++;
  const lineStart = source.lastIndexOf('\n', firstNonWs) + 1;
  const lineEnd = source.indexOf('\n', lineStart);
  if (lineEnd === -1) return null;
  const match = OPENER_RE.exec(source.slice(lineStart, lineEnd));
  return match
    ? { bodyStart: lineEnd + 1, fenceLength: match[1].length }
    : null;
}

function endsWithOuterCloser(
  source: string,
  opener: MarkdownWrapperOpener,
): boolean {
  let end = source.length;
  while (end > opener.bodyStart && /\s/.test(source[end - 1])) end--;
  if (end <= opener.bodyStart) return false;
  const lineStart = source.lastIndexOf('\n', end - 1) + 1;
  if (lineStart < opener.bodyStart) return false;
  const line = source.slice(lineStart, end);
  const match = /^ {0,3}(`+)$/.exec(line);
  return match !== null && match[1].length >= opener.fenceLength;
}

function isOuterCloserPrefix(line: string): boolean {
  return /^ {0,3}`+[\t\r ]*$/.test(line);
}

/**
 * Body projection after a live wrapper has been positively identified.
 *
 * A trailing backtick-only line is withheld even while it is shorter than
 * the outer fence. It may become the wrapper closer on the next byte. If it
 * was an inner closer, incomplete-markdown sealing renders the same code
 * block until following prose proves the line belongs to the body and the
 * withheld bytes append. Up to two trailing blank lines are withheld for the
 * same reason: the settled normalizer drops one padding line before the outer
 * closer, so emitting it early would require a later source shrink.
 */
function streamingWrapperBody(
  source: string,
  opener: MarkdownWrapperOpener,
): string {
  const lines = source.slice(opener.bodyStart).split('\n');
  let start = 0;
  if (lines[start]?.trim() === '') start++;

  let lastNonEmpty = lines.length - 1;
  while (lastNonEmpty >= start && lines[lastNonEmpty].trim() === '') {
    lastNonEmpty--;
  }

  let end = lines.length;
  if (lastNonEmpty >= start && isOuterCloserPrefix(lines[lastNonEmpty])) {
    end = lastNonEmpty;
    if (end > start && lines[end - 1].trim() === '') end--;
  } else {
    let withheldBlankLines = 0;
    while (
      end > start &&
      withheldBlankLines < 2 &&
      lines[end - 1].trim() === ''
    ) {
      end--;
      withheldBlankLines++;
    }
  }
  return lines.slice(start, Math.max(start, end)).join('\n');
}

/**
 * Stateful wrapper normalization for an append-only streaming source. Once
 * its first line cannot become a markdown wrapper, later proven appends return
 * by identity without probing the growing string. A rewrite clears that fact.
 */
export class MarkdownFenceUnwrapper {
  private previousSource = '';
  private previousOutput = '';
  private previousStreaming = false;
  private ruledOut = false;
  private opener: MarkdownWrapperOpener | null = null;
  private streamingUnwrapped = false;
  private append: ProvenAppend | undefined;

  get outputAppend(): ProvenAppend | undefined {
    return this.append;
  }

  render(
    source: string,
    streaming: boolean,
    sourceAppend?: ProvenAppend,
  ): string {
    this.append = undefined;
    const appendProven = matchesProvenAppend(
      sourceAppend,
      this.previousSource,
      source,
    );
    const appendUnproven = !appendProven &&
      source.length > this.previousSource.length &&
      source.startsWith(this.previousSource);
    const sourceRewritten = source !== this.previousSource &&
      !appendProven &&
      !appendUnproven;
    if (sourceRewritten) {
      this.ruledOut = false;
      this.opener = null;
      this.streamingUnwrapped = false;
    }
    if (
      source === this.previousSource &&
      streaming === this.previousStreaming
    ) return this.previousOutput;

    let output = source;
    if (!this.ruledOut) {
      this.opener ??= markdownWrapperOpener(source);
      if (this.streamingUnwrapped && this.opener) {
        output = streaming
          ? streamingWrapperBody(source, this.opener)
          : unwrapMarkdownFence(source);
      } else if (streaming && this.opener && endsWithOuterCloser(source, this.opener)) {
        // This is the one point the old stateless normalizer changed from the
        // outer code block to rich markdown. Make that decision sticky: a
        // same-length inner closer can no longer make the wrapper disappear
        // for one chunk and return when prose follows it.
        const unwrapped = unwrapMarkdownFence(source);
        if (unwrapped !== source) {
          this.streamingUnwrapped = true;
          output = streamingWrapperBody(source, this.opener);
        }
      } else if (!streaming) {
        output = unwrapMarkdownFence(source);
      }
      if (
        output === source &&
        !this.streamingUnwrapped &&
        !openerCanStillMatch(source)
      ) this.ruledOut = true;
    }
    if (
      appendProven &&
      output === source &&
      this.previousOutput === this.previousSource
    ) {
      this.append = sourceAppend;
      output = sourceAppend.next;
    } else if (
      appendProven &&
      this.streamingUnwrapped &&
      output.length > this.previousOutput.length &&
      output.startsWith(this.previousOutput)
    ) {
      this.append = createProvenAppend(
        this.previousOutput,
        output.slice(this.previousOutput.length),
      );
      output = this.append.next;
    }
    this.previousSource = source;
    this.previousOutput = output;
    this.previousStreaming = streaming;
    return output;
  }
}

export function unwrapMarkdownFence(source: string): string {
  if (!source) return source;

  const firstBacktick = source.indexOf('`');
  if (firstBacktick === -1 || firstBacktick > 3) return source;

  // Fast path: test the candidate opener line before paying for the
  // full-source split below. This runs on every reveal tick of a
  // streaming message (ChatMarkdown's processedSource derivation), and
  // the guard above admits any source whose first four characters
  // contain a backtick — most commonly a message that simply STARTS
  // with inline code. Splitting the whole source just to reject those
  // cost O(source) per tick; the sticky test is O(opener line).
  //
  // The candidate opener is the line holding the first non-whitespace
  // character (the first non-empty line, in the split path's terms),
  // which the ≤3 guard confines to the first four characters. The
  // backtick at `firstBacktick` is non-whitespace, so it explicitly
  // bounds the scan.
  let firstNonWs = 0;
  while (firstNonWs < firstBacktick && /\s/.test(source[firstNonWs])) firstNonWs++;
  OPENER_SCAN_RE.lastIndex = source.lastIndexOf('\n', firstNonWs) + 1;
  if (!OPENER_SCAN_RE.test(source)) return source;

  const lines = source.split('\n');

  let firstIdx = -1;
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].trim() !== '') {
      firstIdx = i;
      break;
    }
  }
  if (firstIdx === -1) return source;

  const openerMatch = lines[firstIdx].match(OPENER_RE);
  if (!openerMatch) return source;

  const backtickCount = openerMatch[1].length;

  let lastIdx = -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].trim() !== '') {
      lastIdx = i;
      break;
    }
  }
  if (lastIdx <= firstIdx) return source;

  const closerRe = new RegExp(`^ {0,3}\`{${backtickCount},}\\s*$`);
  if (!closerRe.test(lines[lastIdx])) return source;

  const bodyLines = lines.slice(firstIdx + 1, lastIdx);
  const hasInnerFence = bodyLines.some((line) => /^ {0,3}`{3,}/.test(line));
  if (!hasInnerFence) return source;

  let bodyStart = 0;
  if (bodyStart < bodyLines.length && bodyLines[bodyStart].trim() === '') bodyStart++;

  let bodyEnd = bodyLines.length;
  if (bodyEnd > bodyStart && bodyLines[bodyEnd - 1].trim() === '') bodyEnd--;

  return bodyLines.slice(bodyStart, bodyEnd).join('\n');
}
