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
