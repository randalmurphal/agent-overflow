import { flushSync } from 'svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import { createOpenFocusRequest } from './openFocusRequest.svelte';
import { setPaneLayoutItemsForTest } from '../stores/paneLayout.svelte';
import { focusPane } from '../stores/panes.svelte';

describe('the focus an open asks for', () => {
  beforeEach(() => {
    setPaneLayoutItemsForTest([
      { id: 'tool-issues', paneId: 'tool-issues', kind: 'issues', widthPx: 600 },
      { id: 'ticket-a', paneId: 'ticket-a', kind: 'ticket', widthPx: 600 },
    ]);
    focusPane('tool-issues');
  });

  it('belongs to the one open that asked, until another ask, its taking or a close ends it', () => {
    const request = createOpenFocusRequest<object>();
    const first = {};
    const second = {};
    request.ask(first);
    expect(request.askedFor(first)).toBe(true);
    expect(request.askedFor(second)).toBe(false);
    expect(request.askedFor(null)).toBe(false);
    request.ask(second);
    expect(request.askedFor(first)).toBe(false);
    expect(request.askedFor(second)).toBe(true);
    request.end();
    expect(request.askedFor(second)).toBe(false);
  });

  it('ends on a focus or a press anywhere after the open', () => {
    const request = createOpenFocusRequest<object>();
    const open = {};
    request.ask(open);
    const input = document.body.appendChild(document.createElement('input'));
    input.focus();
    expect(request.askedFor(open)).toBe(false);
    request.ask(open);
    document.body.dispatchEvent(new Event('pointerdown', { bubbles: true }));
    expect(request.askedFor(open)).toBe(false);
    input.remove();
  });

  it('ends on a move to another pane that leaves the page focus where it was', () => {
    const request = createOpenFocusRequest<object>();
    const open = {};
    request.ask(open);
    flushSync();
    expect(request.askedFor(open)).toBe(true);
    focusPane('ticket-a');
    flushSync();
    expect(request.askedFor(open)).toBe(false);
    // Coming back does not bring it back.
    focusPane('tool-issues');
    flushSync();
    expect(request.askedFor(open)).toBe(false);
  });

  it('stops listening once ended', () => {
    const request = createOpenFocusRequest<object>();
    const open = {};
    request.ask(open);
    request.end();
    request.ask(open);
    flushSync();
    // An ask made after the end is the live one; the ended one's watchers
    // are gone, so only this ask's watchers end it.
    expect(request.askedFor(open)).toBe(true);
    focusPane('ticket-a');
    flushSync();
    expect(request.askedFor(open)).toBe(false);
  });
});
