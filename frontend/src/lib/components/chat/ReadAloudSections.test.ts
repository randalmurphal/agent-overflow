// The section layer over a reply being read (ReadAloudSections): nothing for
// an idle reply, the marker on the current section's blocks, "read from
// here" on every other section (one for a block split for length),
// revealed by the pointer over that section,
// nothing drawn (and one report) when the rendered blocks do not match,
// and every listener and observer gone when the reading stops or the row
// unmounts. Real geometry is in e2e/tests/read-aloud-sections.spec.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import AssistantMessage from './AssistantMessage.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { fakeSpeechEngine } from '../../../test/helpers/speech';
import {
  pauseReadAloud,
  readAloudPosition,
  replyReadingKey,
  resetReadAloudForTest,
  stopReadAloud,
  watchReadAloudVoices,
} from '../../stores/readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from '../../stores/readAloudPreferences.svelte';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { speakableSections } from '../../utils/speakableText';

vi.mock('../../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

// Blocks h2 p h2 h2 p; sections 0-1, 2, 3-4. The device reads sentences
// 0-1, 2 and 3-5.
const REPLY = '## One\n\nFirst part here.\n\n## Two\n\n## Three\n\nLast part. Really last.';
const KEY = replyReadingKey('thread-1', 'r1');

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

// A ResizeObserver whose instances the test can see and fire.
class FakeResizeObserver {
  static live = new Set<FakeResizeObserver>();
  static made = 0;
  observed: Element[] = [];
  constructor(private readonly callback: ResizeObserverCallback) {
    FakeResizeObserver.made += 1;
  }
  observe(el: Element): void {
    this.observed.push(el);
    FakeResizeObserver.live.add(this);
  }
  unobserve(): void {}
  disconnect(): void {
    FakeResizeObserver.live.delete(this);
  }
  fire(): void {
    this.callback([], this as unknown as ResizeObserver);
  }
}

describe('<ReadAloudSections>', () => {
  let device: ReturnType<typeof fakeSpeechEngine>;
  // Pointer listeners on reply bodies: added minus removed.
  let pointerListeners: number;

  beforeEach(async () => {
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    setReadAloudPreference('enabled', true);
    resetReadAloudForTest();
    device = fakeSpeechEngine();
    watchReadAloudVoices(device.engine);
    FakeResizeObserver.live.clear();
    FakeResizeObserver.made = 0;
    vi.stubGlobal('ResizeObserver', FakeResizeObserver);
    pointerListeners = 0;
    const add = HTMLElement.prototype.addEventListener;
    const remove = HTMLElement.prototype.removeEventListener;
    vi.spyOn(HTMLElement.prototype, 'addEventListener').mockImplementation(function (this: HTMLElement, type, ...rest) {
      if (type.startsWith('pointer') && this.dataset.testid === 'assistant-message-body') pointerListeners += 1;
      return add.call(this, type, ...rest);
    });
    vi.spyOn(HTMLElement.prototype, 'removeEventListener').mockImplementation(function (this: HTMLElement, type, ...rest) {
      if (type.startsWith('pointer') && this.dataset.testid === 'assistant-message-body') pointerListeners -= 1;
      return remove.call(this, type, ...rest);
    });
    vi.mocked(reportFrontendDiagnostic).mockClear();
    await settle();
  });

  afterEach(() => {
    resetReadAloudForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mount(reply = REPLY, blockCount = 5) {
    const view = render(AssistantMessage, { props: { item: makeItem({ id: 'r1', summary: reply }) } });
    const play = await waitFor(() => view.getByTestId('assistant-message-read-aloud'));
    const blocks = (): HTMLElement[] =>
      Array.from(view.container.querySelector('.markdown-body > .md-committed')!.children) as HTMLElement[];
    expect(blocks()).toHaveLength(blockCount);
    const layer = () => view.queryByTestId('assistant-message-read-aloud-sections');
    const marker = () => view.queryByTestId('assistant-message-read-aloud-marker');
    const fromSections = () =>
      view.queryAllByTestId('assistant-message-read-aloud-from').map((b) => Number(b.parentElement!.dataset.section));
    const reveal = (section: number) =>
      view.getAllByTestId('assistant-message-read-aloud-from').find((b) => b.parentElement!.dataset.section === String(section))!.parentElement!;
    return { view, play, blocks, layer, marker, fromSections, reveal };
  }

  it('mounts nothing, observes nothing and listens for nothing on an idle reply', async () => {
    const { layer, view } = await mount();
    expect(layer()).toBeNull();
    expect(FakeResizeObserver.made).toBe(0);
    expect(pointerListeners).toBe(0);
    await fireEvent.pointerOver(view.container.querySelector('.markdown-body p')!);
    expect(view.queryAllByTestId('assistant-message-read-aloud-from')).toHaveLength(0);
  });

  it('marks the current section’s blocks and offers every other section', async () => {
    const { play, blocks, layer, marker, fromSections } = await mount();
    // Section geometry: block i spans 100i to 100i + 50 from the layer.
    blocks().forEach((el, i) => {
      el.getBoundingClientRect = () => ({ top: 100 * i, bottom: 100 * i + 50, left: 0, right: 0, width: 0, height: 50, x: 0, y: 100 * i, toJSON: () => ({}) });
    });
    await fireEvent.click(play);
    await settle();
    expect(layer()!.dataset.drawn).toBe('true');
    expect(marker()!.dataset.section).toBe('0');
    expect(marker()!.style.top).toBe('0px');
    expect(marker()!.style.height).toBe('150px');
    expect(marker()!.classList.contains('opacity-[0.45]')).toBe(false);
    expect(fromSections()).toEqual([1, 2]);

    // The device reaches section 2 (blocks 3-4).
    device.playbacks[0].onPiece(3);
    await settle();
    expect(marker()!.dataset.section).toBe('2');
    expect(marker()!.style.top).toBe('300px');
    expect(marker()!.style.height).toBe('150px');
    expect(fromSections()).toEqual([0, 1]);

    pauseReadAloud();
    await settle();
    expect(marker()!.classList.contains('opacity-[0.45]')).toBe(true);
    expect(fromSections()).toEqual([0, 1]);
  });

  it('draws on a row mounted while its reply is being read', async () => {
    const first = await mount();
    await fireEvent.click(first.play);
    await settle();
    first.view.unmount();
    expect(FakeResizeObserver.live.size).toBe(0);
    const again = await mount();
    await waitFor(() => expect(again.marker()?.dataset.section).toBe('0'));
    expect(again.fromSections()).toEqual([1, 2]);
    expect(FakeResizeObserver.live.size).toBe(1);
  });

  it('reads from a section’s button, while playing and while paused', async () => {
    const { view, play, marker } = await mount();
    await fireEvent.click(play);
    await settle();
    const from = (section: number) =>
      view.getAllByTestId('assistant-message-read-aloud-from').find((b) => b.parentElement!.dataset.section === String(section))!;
    expect(from(2).getAttribute('aria-label')).toBe('Read from here');
    await fireEvent.click(from(2));
    expect(device.playbacks.at(-1)!.from).toBe(3);
    await settle();
    expect(marker()!.dataset.section).toBe('2');

    pauseReadAloud();
    await settle();
    await fireEvent.click(from(1));
    await settle();
    expect(device.playbacks.at(-1)!.from).toBe(2);
    expect(readAloudPosition(KEY)).toEqual({ section: 1, sections: 3 });
  });

  it('reveals a section’s button while the pointer is over its blocks', async () => {
    const { view, play, blocks, reveal } = await mount();
    await fireEvent.click(play);
    await settle();
    const body = view.getByTestId('assistant-message-body');
    expect(reveal(1).classList.contains('opacity-0')).toBe(true);
    expect(reveal(2).classList.contains('opacity-0')).toBe(true);
    // A touch layout shows every button.
    expect(reveal(1).classList.contains('compact:opacity-100')).toBe(true);

    await fireEvent.pointerOver(blocks()[4].firstChild ?? blocks()[4]);
    expect(reveal(2).classList.contains('opacity-100')).toBe(true);
    expect(reveal(1).classList.contains('opacity-0')).toBe(true);
    await fireEvent.pointerOver(blocks()[2]);
    expect(reveal(1).classList.contains('opacity-100')).toBe(true);
    expect(reveal(2).classList.contains('opacity-0')).toBe(true);
    // Over the current section nothing shows.
    await fireEvent.pointerOver(blocks()[0]);
    expect(reveal(1).classList.contains('opacity-0')).toBe(true);
    await fireEvent.pointerOver(blocks()[3]);
    await fireEvent.pointerLeave(body);
    expect(reveal(2).classList.contains('opacity-0')).toBe(true);
  });

  it('gives a block split for length one button, at its start, for its first part', async () => {
    const long = Array.from({ length: 90 }, () => 'The word goes on here.').join(' ');
    const reply = `A line that is long enough to stand as its own section, well past the short limit.\n\n${long}\n\nTail.`;
    // Sections: the opener (block 0), three parts of block 1, the tail (block 2).
    const sections = speakableSections(reply);
    expect(sections.map((s) => s.firstBlock)).toEqual([0, 1, 1, 1, 2]);
    const firstPiece = (section: number) => sections.slice(0, section).reduce((n, s) => n + s.sentences.length, 0);

    const { view, play, blocks, marker, fromSections, reveal } = await mount(reply, 3);
    await fireEvent.click(play);
    await settle();
    expect(fromSections()).toEqual([1, 4]);

    // Reading the block's second part: its button reads the block from its start.
    device.playbacks[0].onPiece(firstPiece(2));
    await settle();
    expect(marker()!.dataset.section).toBe('2');
    expect(fromSections()).toEqual([0, 1, 4]);
    await fireEvent.pointerOver(blocks()[1]);
    expect(reveal(1).classList.contains('opacity-100')).toBe(true);
    await fireEvent.click(view.getAllByTestId('assistant-message-read-aloud-from')[1]);
    await settle();
    expect(device.playbacks.at(-1)!.from).toBe(firstPiece(1));
    // Reading its first part, the block has no button.
    expect(marker()!.dataset.section).toBe('1');
    expect(fromSections()).toEqual([0, 4]);

    // One pass reads each block once, not once per section sharing it.
    let rects = 0;
    blocks().forEach((el) => {
      const rect = el.getBoundingClientRect.bind(el);
      el.getBoundingClientRect = () => {
        rects += 1;
        return rect();
      };
    });
    const walks = vi.spyOn(document, 'createTreeWalker');
    const [observer] = FakeResizeObserver.live;
    observer.fire();
    expect(rects).toBe(3);
    expect(walks).toHaveBeenCalledTimes(3);
  });

  it('finds a section’s first line from its first character alone', async () => {
    const { play, fromSections, reveal } = await mount('## 🎉 One\n\nBody one.\n\n## Two\n\nBody two.', 4);
    const ranges: Array<{ text: string; start: number; end: number }> = [];
    vi.spyOn(document, 'createRange').mockImplementation(() => {
      const range = { text: '', start: -1, end: -1 };
      ranges.push(range);
      return {
        setStart: (node: Node, at: number) => {
          range.text = node.textContent ?? '';
          range.start = at;
        },
        setEnd: (_node: Node, at: number) => {
          range.end = at;
        },
        getClientRects: () => [{ top: 40 + 100 * ranges.length, height: 20 }],
      } as unknown as Range;
    });
    await fireEvent.click(play);
    await settle();
    expect(fromSections()).toEqual([1]);
    // The pair that makes one character is taken whole.
    expect(ranges).toEqual([{ text: '🎉 One', start: 0, end: 2 }, { text: 'Two', start: 0, end: 1 }]);
    expect(reveal(1).style.top).toBe('240px');
    expect(reveal(1).style.height).toBe('20px');
  });

  it('draws nothing and reports once when the rendered blocks do not match', async () => {
    const { play, blocks, layer, marker, fromSections } = await mount();
    await fireEvent.click(play);
    await settle();
    expect(marker()).not.toBeNull();
    blocks()[4].after(document.createElement('p'));
    const [observer] = FakeResizeObserver.live;
    observer.fire();
    await settle();
    expect(layer()!.dataset.drawn).toBe('false');
    expect(marker()).toBeNull();
    expect(fromSections()).toEqual([]);
    observer.fire();
    device.playbacks[0].onPiece(2);
    await settle();
    expect(marker()).toBeNull();
    expect(reportFrontendDiagnostic).toHaveBeenCalledTimes(1);
    expect(reportFrontendDiagnostic).toHaveBeenCalledWith(
      'Read aloud: the reply renders other blocks than it is read in',
      'rendered 6, expected 5',
    );
    // The meta-row controls still move the reading.
    expect(readAloudPosition(KEY)?.section).toBe(1);
  });

  it('waits without a report while a volatile tail renders, and draws once it settles', async () => {
    const { view, play, layer, marker } = await mount();
    const root = view.container.querySelector('.markdown-body')!;
    const tail = document.createElement('div');
    tail.className = 'md-volatile';
    root.append(tail);
    await fireEvent.click(play);
    await settle();
    expect(layer()!.dataset.drawn).toBe('false');
    expect(reportFrontendDiagnostic).not.toHaveBeenCalled();
    tail.remove();
    await waitFor(() => expect(marker()).not.toBeNull());
    expect(reportFrontendDiagnostic).not.toHaveBeenCalled();
  });

  it('removes its listeners and observer when the reading stops and when the row unmounts, and never doubles them', async () => {
    const { view, play, layer } = await mount();
    const body = view.getByTestId('assistant-message-body');
    await fireEvent.click(play);
    await settle();
    expect(pointerListeners).toBe(2);
    expect(FakeResizeObserver.live.size).toBe(1);
    expect([...FakeResizeObserver.live][0].observed).toEqual([body]);

    // Moving and pausing keep the one set.
    device.playbacks[0].onPiece(2);
    pauseReadAloud();
    await settle();
    expect(pointerListeners).toBe(2);
    expect(FakeResizeObserver.live.size).toBe(1);

    stopReadAloud();
    await settle();
    expect(layer()).toBeNull();
    expect(pointerListeners).toBe(0);
    expect(FakeResizeObserver.live.size).toBe(0);

    await fireEvent.click(play);
    await settle();
    await fireEvent.click(play);
    await fireEvent.click(play);
    await settle();
    expect(pointerListeners).toBe(2);
    expect(FakeResizeObserver.live.size).toBe(1);

    view.unmount();
    expect(pointerListeners).toBe(0);
    expect(FakeResizeObserver.live.size).toBe(0);
  });
});
