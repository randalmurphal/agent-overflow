// The hardware back stack, rung by rung. The plugin is not here — the
// listener in `installNativeLifecycle` is one line over `answerBackPress`,
// and this suite drives that function directly with the stores a phone
// session would have: the compact layout, a thread pane, a companion,
// and the DOM the strip renders them into.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { answerBackPress } from './lifecycle';
import {
  getCompactScreen,
  setCompactLayoutForTest,
  showCompactList,
  showCompactThread,
} from '../stores/layoutMode.svelte';
import { createPane, focusPane, resetPanesForTest } from '../stores/panes.svelte';
import { REVEAL_PANE_EVENT } from '../stores/eventNames';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from '../stores/paneLayout.svelte';
import {
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from '../stores/companionPanes.svelte';

function threadItem(paneId: string): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx: 412 };
}

/** The strip as compact renders it: every pane a full-width section, and
 * the one under the strip's centre is the one on screen. happy-dom has no
 * layout, so each section states its own horizontal extent. */
function mountStrip(sections: { paneId: string; kind: string; onScreen: boolean }[]): void {
  const strip = document.createElement('div');
  strip.className = 'compact-screen-thread';
  strip.getBoundingClientRect = () => rect(0, 412);
  for (const { paneId, kind, onScreen } of sections) {
    const section = document.createElement('section');
    section.dataset.paneId = paneId;
    section.dataset.paneKind = kind;
    section.getBoundingClientRect = () => (onScreen ? rect(0, 412) : rect(412, 824));
    strip.appendChild(section);
  }
  document.body.appendChild(strip);
}

function rect(left: number, right: number): DOMRect {
  return { left, right, top: 0, bottom: 800, width: right - left, height: 800, x: left, y: 0, toJSON: () => ({}) };
}

beforeEach(() => {
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
  setCompactLayoutForTest(true);
  showCompactThread();
});

afterEach(() => {
  document.body.innerHTML = '';
  setCompactLayoutForTest(false);
  showCompactList();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('answerBackPress', () => {
  it('lets an open surface consume the press first, and changes nothing else', () => {
    const consume = (event: Event) => event.preventDefault();
    window.addEventListener('keydown', consume);
    try {
      expect(answerBackPress()).toBe(true);
      expect(getCompactScreen()).toBe('thread');
    } finally {
      window.removeEventListener('keydown', consume);
    }
  });

  it('closes a terminal drawer stacked over the chat before anything moves', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    const pane = createPane('main');
    focusPane('main');
    pane.setShowTerminal(true);

    expect(answerBackPress()).toBe(true);
    expect(pane.showTerminal).toBe(false);
    expect(getCompactScreen()).toBe('thread');
  });

  it('sends a focused terminal\'s Escape to the pane, never into xterm', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    mountStrip([{ paneId: 'main', kind: 'thread', onScreen: true }]);
    const section = document.querySelector<HTMLElement>('[data-pane-id="main"]')!;
    const xterm = document.createElement('div');
    xterm.className = 'xterm';
    const textarea = document.createElement('textarea');
    xterm.appendChild(textarea);
    section.appendChild(xterm);
    textarea.focus();
    let reachedXterm = false;
    textarea.addEventListener('keydown', () => {
      reachedXterm = true;
    });
    let sawAtPane: EventTarget | null = null;
    section.addEventListener('keydown', (event) => {
      sawAtPane = event.target;
    });

    answerBackPress();
    expect(reachedXterm).toBe(false);
    expect(sawAtPane).toBe(section);
  });

  it('closes the companion on screen and brings its thread back', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    const review = openCompanion('main', 'review')!;
    mountStrip([
      { paneId: 'main', kind: 'thread', onScreen: false },
      { paneId: review.paneId, kind: 'review', onScreen: true },
    ]);
    const revealed: string[] = [];
    window.addEventListener(REVEAL_PANE_EVENT, ((event: CustomEvent<{ paneId: string }>) => {
      revealed.push(event.detail.paneId);
    }) as EventListener);

    expect(answerBackPress()).toBe(true);
    expect(isCompanionOpen('main', 'review')).toBe(false);
    expect(revealed).toEqual(['main']);
    expect(getCompactScreen()).toBe('thread');
  });

  it('goes from a thread with nothing stacked on it to the list', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    mountStrip([{ paneId: 'main', kind: 'thread', onScreen: true }]);

    expect(answerBackPress()).toBe(true);
    expect(getCompactScreen()).toBe('list');
  });

  it('answers false from the list, which is the root, and off compact', () => {
    showCompactList();
    expect(answerBackPress()).toBe(false);

    setCompactLayoutForTest(false);
    showCompactThread();
    expect(answerBackPress()).toBe(false);
  });
});
