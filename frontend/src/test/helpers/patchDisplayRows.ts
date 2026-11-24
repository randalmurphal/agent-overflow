// The display rows of a parsed patch, built from its lines in one pass:
// gap rows for the hidden ranges, line numbers per side, stable row ids,
// and intraline ranges on paired del/add rows. The review surface builds
// its rows from the compact store in utils/patchRows.ts; this is the
// reference that tests check that store and the streaming parser
// (patchParser.test.ts) against.
import { intralineRanges } from '../../lib/utils/intralineDiff';
import { parseHunkHeader, stripPatchLinePrefix, type DiffGap, type PatchDisplayRow, type PatchLine } from '../../lib/utils/patchFiles';

export function buildPatchDisplayRows(lines: PatchLine[], newSideTotal?: number, suppressGaps = false): PatchDisplayRow[] {
  const rows: PatchDisplayRow[] = [];
  let oldLine = 0;
  let newLine = 0;
  let fallbackIndex = 0;
  // Conflict pseudo-files represent hidden runs as their own fold rows
  // (utils/conflictFile.ts) — emitting hunk gaps there would double up.
  const emitGaps = !suppressGaps && !lines.some((line) => line.fold !== undefined || line.type === 'marker');
  let gapId = 0;
  let sawHunk = false;
  // oldStart of the first hunk: 0 marks an added file (fully present,
  // nothing to expand at either end).
  let firstOldStart = 0;
  let lastNewStart = 0;

  function pushGap(startNew: number, endNew: number, hidden: number, location: DiffGap['location']): void {
    rows.push({
      id: `gap:${gapId}:${startNew}`,
      line: { content: '', type: 'context' },
      oldLine: 0,
      newLine: 0,
      side: 'context',
      gap: { id: gapId, startNew, endNew, hidden, location },
    });
    gapId += 1;
  }

  for (const line of lines) {
    if (line.type === 'meta') {
      const hunk = parseHunkHeader(line.content);
      if (hunk) {
        if (emitGaps) {
          if (!sawHunk) {
            firstOldStart = hunk.oldStart;
            // Leading gap: unchanged lines above the first hunk.
            if (hunk.oldStart > 1 && hunk.newStart > 1) {
              pushGap(1, hunk.newStart - 1, hunk.newStart - 1, 'leading');
            }
          } else if (hunk.newStart > 0 && hunk.newStart > newLine) {
            // Between-hunks gap. Old/new hidden counts are equal here
            // (only unchanged lines separate hunks).
            pushGap(newLine, hunk.newStart - 1, hunk.newStart - newLine, 'between');
          }
          sawHunk = true;
          lastNewStart = hunk.newStart;
        }
        oldLine = hunk.oldStart;
        newLine = hunk.newStart;
      }
      continue;
    }

    // Conflict marker/fold rows display (unlike meta) but carry no line
    // numbers and advance neither side — a fold's skipped span is applied
    // by the hunk header that follows it.
    if (line.type === 'marker') {
      rows.push({
        id: `${rows.length}:0:0:${fallbackIndex}`,
        line,
        oldLine: 0,
        newLine: 0,
        side: 'context',
      });
      fallbackIndex += 1;
      continue;
    }

    let rowOldLine = 0;
    let rowNewLine = 0;
    let side: PatchDisplayRow['side'] = 'context';

    if (line.type === 'del') {
      rowOldLine = oldLine;
      oldLine += 1;
      side = 'old';
    } else if (line.type === 'add') {
      rowNewLine = newLine;
      newLine += 1;
      side = 'new';
    } else {
      rowOldLine = oldLine;
      rowNewLine = newLine;
      oldLine += 1;
      newLine += 1;
    }

    rows.push({
      id: `${rows.length}:${rowOldLine}:${rowNewLine}:${fallbackIndex}`,
      line,
      oldLine: rowOldLine,
      newLine: rowNewLine,
      side,
    });
    fallbackIndex += 1;
  }

  // Trailing gap: a modified file usually continues past its last
  // hunk. Skipped for added files (fully present), deleted files (no
  // new side), and once a known total says the last hunk reached EOF.
  if (emitGaps && sawHunk && firstOldStart > 0 && lastNewStart > 0) {
    if (newSideTotal === undefined) {
      pushGap(newLine, -1, -1, 'trailing');
    } else if (newSideTotal >= newLine) {
      pushGap(newLine, newSideTotal, newSideTotal - newLine + 1, 'trailing');
    }
  }

  attachIntralineRanges(rows);
  return rows;
}

/**
 * Pair the i-th deleted line with the i-th added line inside each
 * del-run → add-run block (split view's pairing) and stamp intraline
 * changed ranges onto both rows.
 */
function attachIntralineRanges(rows: PatchDisplayRow[]): void {
  let index = 0;
  while (index < rows.length) {
    if (rows[index]?.line.type !== 'del') {
      index += 1;
      continue;
    }
    const dels: PatchDisplayRow[] = [];
    while (rows[index]?.line.type === 'del') {
      dels.push(rows[index]);
      index += 1;
    }
    const adds: PatchDisplayRow[] = [];
    while (rows[index]?.line.type === 'add') {
      adds.push(rows[index]);
      index += 1;
    }
    const pairs = Math.min(dels.length, adds.length);
    for (let k = 0; k < pairs; k += 1) {
      const ranges = intralineRanges(
        stripPatchLinePrefix(dels[k].line),
        stripPatchLinePrefix(adds[k].line),
      );
      if (!ranges) continue;
      if (ranges.del.end > ranges.del.start) dels[k].intraline = ranges.del;
      if (ranges.add.end > ranges.add.start) adds[k].intraline = ranges.add;
    }
  }
}
