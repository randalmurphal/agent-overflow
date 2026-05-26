const OPENER_RE = /^ {0,3}(`{3,})(markdown|md)\s*$/i;

export function unwrapMarkdownFence(source: string): string {
  if (!source) return source;

  const firstBacktick = source.indexOf('`');
  if (firstBacktick === -1 || firstBacktick > 3) return source;

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
