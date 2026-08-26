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

function row(id: string, kind: string, role: string, status: string, text: string): string {
  return `
    <div data-row-index="${id.length}">
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
            ${row('i1', 'user_text', 'user', 'completed', 'How do I sort an array?')}
            ${row('i2', 'assistant_text', 'assistant', 'streaming', 'Use   Array.prototype.sort')}
            ${row('i3', 'tool_call', 'assistant', 'running', 'Bash')}
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
        ${row('i1', 'assistant_text', 'assistant', 'completed', 'text')}
      </section>`);
    const pane = readViewport(doc, { sinceMutationMs: 0 }).panes[0]!;
    expect(pane.mountedRows).toBe(2);
    expect(pane.rows.map((r) => r.itemId)).toEqual(['i1']);
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
