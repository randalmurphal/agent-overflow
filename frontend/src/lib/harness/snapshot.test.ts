// The snapshot's pure helpers plus the DOM walk over a hand-built tree.
//
// happy-dom has no layout engine, so every getBoundingClientRect answers
// a zero box. That is fine and deliberate: what this suite pins is the
// SEMANTIC half — which rows are found, in what order, with which
// discriminators and how their text is capped — because that is the half
// a reader of the snapshot actually consumes. Geometry is exercised end
// to end by e2e/tests/harness-bridge.spec.ts against a real browser.

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_TEXT_HEAD,
  MAX_ROWS_PER_PANE,
  readElement,
  readScroll,
  readViewport,
  rectsOverlapVertically,
  textHead,
} from './snapshot';

function html(markup: string): Document {
  document.body.innerHTML = markup;
  return document;
}

// The row index is a PARAMETER because it is part of what the snapshot
// reports: a reader diffing two viewports uses it to say which window of a
// virtualized list is mounted. Deriving it from the id (as this helper
// once did) made it unassertable.
function row(
  index: number,
  id: string,
  kind: string,
  role: string,
  status: string,
  text: string,
): string {
  return `
    <div data-row-index="${index}">
      <div data-item-id="${id}" data-item-kind="${kind}" data-item-role="${role}" data-item-status="${status}">
        ${status === 'running' ? '<span data-testid="indicator" data-state="running"></span>' : ''}
        <p>${text}</p>
      </div>
    </div>`;
}

describe('textHead', () => {
  it('collapses whitespace so a row occupies one terminal line', () => {
    expect(textHead('  hello\n\n  world  \t there ')).toBe('hello world there');
  });

  it('caps at the requested length and marks the truncation', () => {
    expect(textHead('abcdefghij', 4)).toBe('abcd…');
    expect(textHead('abcd', 4)).toBe('abcd');
  });

  // Slicing UTF-16 units would leave a lone surrogate, which serialises
  // fine and renders as a replacement char that looks like real content.
  it('counts code points, never splitting a surrogate pair', () => {
    const capped = textHead('👩‍🚀🚀🚀🚀', 2);
    expect(Array.from(capped)).toHaveLength(3); // two points plus the ellipsis
    expect(capped.includes('�')).toBe(false);
  });

  it('answers empty for a zero cap', () => {
    expect(textHead('anything', 0)).toBe('');
  });

  // The point array is built over a `cap * 2`-unit prefix rather than the
  // whole string, so the bounded slice itself can land mid-pair. It may
  // only ever do so PAST the cap, which the point slice drops.
  it('bounds the work it does without orphaning a surrogate at the bound', () => {
    const capped = textHead(`a${'👩'.repeat(50)}`, 4);
    expect(Array.from(capped)).toHaveLength(5); // four points plus the ellipsis
    expect(capped.includes('�')).toBe(false);
    expect(capped).toBe('a👩👩👩…');
  });
});

describe('readScroll', () => {
  it('reports distance from the bottom and tolerates sub-pixel offsets', () => {
    expect(readScroll({ scrollTop: 900, scrollHeight: 1000, clientHeight: 100 })).toMatchObject({
      distanceFromBottom: 0,
      atBottom: true,
    });
    expect(readScroll({ scrollTop: 899.4, scrollHeight: 1000, clientHeight: 100 }).atBottom).toBe(
      true,
    );
    expect(readScroll({ scrollTop: 500, scrollHeight: 1000, clientHeight: 100 })).toMatchObject({
      distanceFromBottom: 400,
      atBottom: false,
    });
  });
});

describe('rectsOverlapVertically', () => {
  const viewport = { x: 0, y: 100, w: 800, h: 400 };
  it('accepts a partially visible row and rejects one entirely above', () => {
    expect(rectsOverlapVertically({ x: 0, y: 80, w: 800, h: 40 }, viewport)).toBe(true);
    expect(rectsOverlapVertically({ x: 0, y: 0, w: 800, h: 40 }, viewport)).toBe(false);
    expect(rectsOverlapVertically({ x: 0, y: 600, w: 800, h: 40 }, viewport)).toBe(false);
  });
});

describe('readViewport', () => {
  it('reads panes, rows and their discriminators in document order', () => {
    const doc = html(`
      <section data-pane-id="pane-a" data-pane-kind="chat" data-pane-focused="true">
        <div data-ui-surface="chat" data-thread-id="thread-1">
          <div data-testid="message-timeline-scroll">
            ${row(12, 'i1', 'user_text', 'user', 'completed', 'How do I sort an array?')}
            ${row(13, 'i2', 'assistant_text', 'assistant', 'streaming', 'Use   Array.prototype.sort')}
            ${row(14, 'i3', 'tool_call', 'assistant', 'running', 'Bash')}
          </div>
        </div>
      </section>
      <section data-pane-id="pane-b" data-pane-kind="chat" data-pane-focused="false">
        <div data-ui-surface="chat" data-thread-id="thread-2"></div>
      </section>`);

    const snapshot = readViewport(doc, { sinceMutationMs: 999 });
    expect(snapshot.v).toBe(1);
    expect(snapshot.settled).toBe(true);
    expect(snapshot.activeThreadId).toBe('thread-1');
    expect(snapshot.panes.map((p) => p.paneId)).toEqual(['pane-a', 'pane-b']);

    const pane = snapshot.panes[0]!;
    expect(pane.focused).toBe(true);
    expect(pane.mountedRows).toBe(3);
    expect(pane.rows.map((r) => r.itemId)).toEqual(['i1', 'i2', 'i3']);
    // The virtualizer's own indices, not positions in this array: a
    // snapshot of a scrolled timeline starts partway into the list.
    expect(pane.rows.map((r) => r.rowIndex)).toEqual([12, 13, 14]);
    expect(pane.rows[1]).toMatchObject({
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      streaming: true,
      textHead: 'Use Array.prototype.sort',
    });
    expect(pane.rows[2]!.badge).toBe('running');
    expect(pane.rows[0]!.streaming).toBe(false);
    expect(pane.rows[0]!.badge).toBe('');

    // No timeline in pane B, so no scroll block at all rather than a
    // block full of zeros that reads like a scrolled-to-top pane.
    expect(snapshot.panes[1]!.scroll).toBeNull();
    expect(snapshot.panes[1]!.threadId).toBe('thread-2');
  });

  it('reports unsettled while the document is still changing', () => {
    const doc = html('<section data-pane-id="p"></section>');
    expect(readViewport(doc, { sinceMutationMs: 40 }).settled).toBe(false);
    expect(readViewport(doc, { sinceMutationMs: 40, settledMs: 10 }).settled).toBe(true);
  });

  it('names open overlays by their accessible name', () => {
    const doc = html(`
      <div role="dialog" aria-label="Settings"></div>
      <div role="dialog" aria-labelledby="t"><h2 id="t">Discard run</h2></div>
      <div data-popover data-testid="model-picker"></div>`);
    expect(readViewport(doc, { sinceMutationMs: 0 }).overlays).toEqual([
      { name: 'Settings', kind: 'dialog', rect: { x: 0, y: 0, w: 0, h: 0 } },
      { name: 'Discard run', kind: 'dialog', rect: { x: 0, y: 0, w: 0, h: 0 } },
      { name: 'model-picker', kind: 'popover', rect: { x: 0, y: 0, w: 0, h: 0 } },
    ]);
  });

  // A structural row (a response divider) carries data-row-index but no
  // leaf. Emitting it as a row with an empty id would break every diff
  // that keys on itemId.
  it('skips a mounted wrapper that holds no item', () => {
    const doc = html(`
      <section data-pane-id="p">
        <div data-row-index="0"><div data-testid="response-divider"></div></div>
        ${row(1, 'i1', 'assistant_text', 'assistant', 'completed', 'text')}
      </section>`);
    const pane = readViewport(doc, { sinceMutationMs: 0 }).panes[0]!;
    expect(pane.mountedRows).toBe(2);
    expect(pane.rows.map((r) => r.itemId)).toEqual(['i1']);
  });

  // The cap is plumbed from the CLI's `--text-head`, and for a long time it
  // was accepted at the door and then dropped: every row came back at the
  // default no matter what was asked for.
  it('applies a caller-supplied text cap to every row', () => {
    const long = 'y'.repeat(DEFAULT_TEXT_HEAD * 2);
    const doc = html(`
      <section data-pane-id="p">
        ${row(0, 'i1', 'assistant_text', 'assistant', 'completed', long)}
        ${row(1, 'i2', 'assistant_text', 'assistant', 'completed', long)}
      </section>`);

    const capped = readViewport(doc, { sinceMutationMs: 0, textHead: 10 }).panes[0]!;
    expect(capped.rows.map((r) => r.textHead)).toEqual(['yyyyyyyyyy…', 'yyyyyyyyyy…']);

    // 0 is what the int flag holds when the caller did not pass one, and
    // absence means the same thing.
    for (const opts of [{ sinceMutationMs: 0 }, { sinceMutationMs: 0, textHead: 0 }]) {
      const row0 = readViewport(doc, opts).panes[0]!.rows[0]!;
      expect(row0.textHead).toHaveLength(DEFAULT_TEXT_HEAD + 1);
    }
  });

  // A pane cannot plausibly mount 400+ rows; a runaway is a finding, and
  // dumping it into a terminal-read document would bury the finding.
  it('truncates a runaway pane at the row ceiling while still counting them all', () => {
    const rows: string[] = [];
    for (let i = 0; i < MAX_ROWS_PER_PANE + 25; i += 1) {
      rows.push(row(i, `i${i}`, 'assistant_text', 'assistant', 'completed', `row ${i}`));
    }
    const doc = html(`<section data-pane-id="p">${rows.join('')}</section>`);
    const pane = readViewport(doc, { sinceMutationMs: 0 }).panes[0]!;
    expect(pane.rows).toHaveLength(MAX_ROWS_PER_PANE);
    expect(pane.mountedRows).toBe(MAX_ROWS_PER_PANE + 25);
    expect(pane.rows.at(-1)!.rowIndex).toBe(MAX_ROWS_PER_PANE - 1);
  });
});

describe('readElement', () => {
  it('counts matches and describes the first', () => {
    const doc = html(`
      <button data-testid="send" aria-label="Send message" role="button">Send</button>
      <button data-testid="send">Send again</button>`);
    const result = readElement(doc, '[data-testid="send"]');
    expect(result).toMatchObject({
      v: 1,
      selector: '[data-testid="send"]',
      count: 2,
    });
    expect(result.first).toMatchObject({
      tag: 'button',
      text: 'Send',
      role: 'button',
      ariaLabel: 'Send message',
      testId: 'send',
    });
  });

  it('answers a miss with a null first rather than an error', () => {
    const result = readElement(html('<div></div>'), '.nothing-here');
    expect(result.count).toBe(0);
    expect(result.first).toBeNull();
  });

  it('caps the text it returns', () => {
    const doc = html(`<p id="long">${'x'.repeat(900)}</p>`);
    expect(readElement(doc, '#long', 20).first?.text).toHaveLength(21);
  });
});
