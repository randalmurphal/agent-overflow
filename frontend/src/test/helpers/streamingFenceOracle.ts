export interface StreamingFenceExpectation {
  texts: string[];
  hasOpenFence: boolean;
}

function stripPartialCloser(
  body: string,
  marker: string,
  fenceLength: number,
): string {
  const lastNewline = body.lastIndexOf('\n');
  if (lastNewline < 0) return body;
  const lastLine = body
    .slice(lastNewline + 1)
    .replace(/^[ \t]*(?:>[ \t]*)*/, '');
  if (
    lastLine.length > 0 &&
    lastLine.length < fenceLength &&
    Array.from(lastLine).every((character) => character === marker)
  ) {
    return body.slice(0, lastNewline);
  }
  return body;
}

/** Independent top-level fence oracle for streaming-render tests. */
export function expectedStreamingFenceTexts(source: string): StreamingFenceExpectation {
  const texts: string[] = [];
  let lineStart = 0;
  while (lineStart < source.length) {
    let lineEnd = source.indexOf('\n', lineStart);
    const hasNewline = lineEnd !== -1;
    if (!hasNewline) lineEnd = source.length;

    let cursor = lineStart;
    let indent = 0;
    while (cursor < lineEnd && source.charCodeAt(cursor) === 32 && indent < 4) {
      cursor++;
      indent++;
    }
    const marker = source[cursor];
    let runEnd = cursor;
    if ((marker === '`' || marker === '~') && indent <= 3) {
      while (runEnd < lineEnd && source[runEnd] === marker) runEnd++;
    }
    const fenceLength = runEnd - cursor;
    let opener = fenceLength >= 3;
    if (opener && marker === '`' && source.slice(runEnd, lineEnd).includes('`')) {
      opener = false;
    }
    if (!opener) {
      if (!hasNewline) break;
      lineStart = lineEnd + 1;
      continue;
    }

    const bodyStart = hasNewline ? lineEnd + 1 : lineEnd;
    let candidateStart = bodyStart;
    let closed = false;
    while (candidateStart <= source.length) {
      let candidateEnd = source.indexOf('\n', candidateStart);
      const candidateHasNewline = candidateEnd !== -1;
      if (!candidateHasNewline) candidateEnd = source.length;
      let closeCursor = candidateStart;
      let closeIndent = 0;
      while (
        closeCursor < candidateEnd &&
        source.charCodeAt(closeCursor) === 32 &&
        closeIndent < 4
      ) {
        closeCursor++;
        closeIndent++;
      }
      let closeRunEnd = closeCursor;
      while (closeRunEnd < candidateEnd && source[closeRunEnd] === marker) {
        closeRunEnd++;
      }
      let validCloser = closeIndent <= 3 && closeRunEnd - closeCursor >= fenceLength;
      for (let index = closeRunEnd; validCloser && index < candidateEnd; index++) {
        const code = source.charCodeAt(index);
        if (code !== 9 && code !== 13 && code !== 32) validCloser = false;
      }
      if (validCloser) {
        let bodyEnd = candidateStart;
        if (bodyEnd > bodyStart && source.charCodeAt(bodyEnd - 1) === 10) bodyEnd--;
        if (bodyEnd > bodyStart && source.charCodeAt(bodyEnd - 1) === 13) bodyEnd--;
        texts.push(source.slice(bodyStart, bodyEnd));
        lineStart = candidateHasNewline ? candidateEnd + 1 : source.length;
        closed = true;
        break;
      }
      if (!candidateHasNewline) break;
      candidateStart = candidateEnd + 1;
    }
    if (!closed) {
      texts.push(
        stripPartialCloser(source.slice(bodyStart), marker, fenceLength)
          .trimEnd(),
      );
      return { texts, hasOpenFence: true };
    }
  }
  return { texts, hasOpenFence: false };
}
