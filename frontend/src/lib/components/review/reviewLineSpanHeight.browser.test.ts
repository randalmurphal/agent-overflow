import { describe, it, expect, afterEach } from 'vitest';
// Import the REAL production stylesheet so the assertions run against
// the actual `.syntax-<name>` rules (src/styles/syntax.css, pulled in
// by app.css). The review pane's exact-height contract assumes those
// rules are inline-only: color / font-weight / font-style, never
// vertical geometry. A future palette edit that adds padding, margin,
// display, or line-height to a syntax class would misplace every
// virtualized review row below it — this file is the tripwire.
import '../../../app.css';
import { REVIEW_LINE_HEIGHT_PX } from '../../utils/reviewRows';

// Keep in sync with the rule list in src/styles/syntax.css (the
// classId → name table comes from internal/highlight/classids.go).
const SYNTAX_CLASSES = [
  'syntax-keyword',
  'syntax-string',
  'syntax-string-special',
  'syntax-comment',
  'syntax-number',
  'syntax-function',
  'syntax-type',
  'syntax-variable-builtin',
  'syntax-property',
  'syntax-constant',
  'syntax-tag',
  'syntax-attribute',
  'syntax-namespace',
  'syntax-label',
  'syntax-markup-heading',
  'syntax-markup-bold',
  'syntax-markup-italic',
  'syntax-markup-link',
  'syntax-markup-raw',
  'syntax-markup-list',
  'syntax-markup-quote',
  'syntax-added',
  'syntax-removed',
];

const mounted: HTMLElement[] = [];

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
});

// A faithful slice of ReviewLineBlockRow's stacked line row: the block
// pins line-height via the shared constant, the row is a flex line,
// and the content cell hosts DiffLineContent's inline spans. The row
// deliberately has NO fixed height here — the assertion is that the
// content fits 20px naturally, not that overflow-hidden clips it.
function mountReviewLine(children: (HTMLElement | string)[]): HTMLElement {
  const block = document.createElement('div');
  block.className = 'bg-surface-1 font-mono text-xs text-fg';
  block.style.lineHeight = `${REVIEW_LINE_HEIGHT_PX}px`;
  block.style.width = '600px';

  const row = document.createElement('div');
  row.className = 'flex';
  const content = document.createElement('span');
  content.className = 'min-w-0 flex-1 whitespace-pre pl-2 pr-3';
  for (const child of children) {
    content.append(child);
  }
  row.appendChild(content);
  block.appendChild(row);
  document.body.appendChild(block);
  mounted.push(block);
  return row;
}

function span(className: string, text: string): HTMLElement {
  const el = document.createElement('span');
  el.className = className;
  el.textContent = text;
  return el;
}

describe('review exact-height contract with syntax spans', () => {
  it('every syntax class renders a review line at exactly REVIEW_LINE_HEIGHT_PX', () => {
    for (const className of SYNTAX_CLASSES) {
      const row = mountReviewLine([span(className, 'const x = 1;')]);
      expect.soft(row.offsetHeight, className).toBe(REVIEW_LINE_HEIGHT_PX);
      expect.soft(row.scrollHeight, className).toBe(REVIEW_LINE_HEIGHT_PX);
    }
  });

  it('a mixed line (prefix tint + spans + intraline emph wash) stays one line tall', () => {
    // DiffLineContent's densest shape: colored `+` prefix, keyword and
    // string spans, and an emphasized intraline segment carrying the
    // rounded background wash.
    const emph = span('syntax-string rounded-[2px] bg-success/35', 'baz');
    const row = mountReviewLine([
      span('text-success', '+'),
      span('syntax-keyword', 'const'),
      ' value = ',
      span('syntax-string', '"bar'),
      emph,
      span('syntax-string', '"'),
      ';',
    ]);
    expect(row.offsetHeight).toBe(REVIEW_LINE_HEIGHT_PX);
    expect(row.scrollHeight).toBe(REVIEW_LINE_HEIGHT_PX);
  });
});
