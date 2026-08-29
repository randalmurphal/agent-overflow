import { render } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type { SmoothingClock } from '../../markdown/smoothing/PerItemSmoother';
import {
  __setSmoothingClockForTest,
} from '../../stores/thread.svelte';
import { getSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import { setViewOnlySessionFromBootstrap } from '../../transport/runMode';
import {
  __resetHarnessModeForTest,
  setHarnessSessionFromBootstrap,
} from '../../transport/harnessMode';
import {
  buildPane,
  installThreadSwitchMocks,
  makeItem,
  makeThread,
} from '../../../test/helpers/chat';
import TimelineLeaf from './TimelineLeaf.svelte';

class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();

  now(): number {
    return this.current;
  }

  schedule(callback: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, callback);
    return handle;
  }

  cancel(handle: number): void {
    this.pending.delete(handle);
  }

  tickFrame(milliseconds: number): void {
    this.current += milliseconds;
    const callbacks = [...this.pending.values()];
    this.pending.clear();
    for (const callback of callbacks) callback();
  }

  hasPendingFrame(): boolean {
    return this.pending.size > 0;
  }
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
  setViewOnlySessionFromBootstrap(false);
  __resetHarnessModeForTest();
});

describe('assistant streaming reveal integration', () => {
  it('does not expose Markdown source state in an ordinary session', async () => {
    const item = makeItem({
      id: 'ordinary-answer',
      threadId: 'ordinary-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: 'ordinary answer',
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'ordinary-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();
    const body = view.getByTestId('assistant-message-body') as HTMLElement & {
      __aoMarkdownForensics?: unknown;
    };
    expect(body.__aoMarkdownForensics).toBeUndefined();
  });

  it('exposes source-owning Markdown forensics only in a harness session', async () => {
    setHarnessSessionFromBootstrap(true);
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'forensic-streaming-answer',
      threadId: 'forensic-streaming-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'forensic-streaming-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const appendAndDrain = async (delta: string, updatedAt: number) => {
      pane.applyItemDelta({
        threadId: item.threadId,
        itemId: item.id,
        kind: 'assistant_text',
        delta,
        updatedAt,
      });
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };

    await appendAndDrain('first words ', 2);
    await appendAndDrain('paint directly ', 3);

    const body = view.getByTestId('assistant-message-body') as HTMLElement & {
      __aoMarkdownForensics?: {
        itemId: string;
        canonicalSource: string;
        parserSource: string;
        renderedSource: string;
        streaming: boolean;
      };
    };
    const forensics = body.__aoMarkdownForensics;
    expect(forensics).toBeDefined();
    expect(forensics?.itemId).toBe(item.id);
    expect(forensics?.canonicalSource).toBe('first words paint directly ');
    expect(forensics?.parserSource).toBe('first words ');
    expect(forensics?.renderedSource).toBe('first words ');
    expect(forensics?.streaming).toBe(true);
    expect(Object.keys(body)).not.toContain('__aoMarkdownForensics');
    const streamdown = body.querySelector<HTMLElement & {
      __aoStreamdownForensics?: {
        readonly content: string;
        readonly lastPath: string;
        readonly incrementalLexMetrics: {
          readonly calls: number;
          readonly byPath: Record<string, { readonly calls: number }>;
        };
      };
    }>('.md-volatile');
    expect(streamdown?.__aoStreamdownForensics?.content).toBe('first words ');
    expect(streamdown?.__aoStreamdownForensics?.lastPath).not.toBe('none');
    expect(streamdown?.__aoStreamdownForensics?.incrementalLexMetrics.calls)
      .toBeGreaterThan(0);
    expect(streamdown?.__aoStreamdownForensics?.incrementalLexMetrics.byPath.full.calls)
      .toBeGreaterThan(0);
    expect(Object.keys(streamdown ?? {})).not.toContain('__aoStreamdownForensics');
  });

  it('restores a remounted representation while another view keeps streaming', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'shared-streaming-answer',
      threadId: 'shared-streaming-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'shared-streaming-pane',
    );
    const keeper = render(TimelineLeaf, { props: { pane, item } });
    const firstMount = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const appendAndDrain = async (delta: string, updatedAt: number) => {
      pane.applyItemDelta({
        threadId: item.threadId,
        itemId: item.id,
        kind: 'assistant_text',
        delta,
        updatedAt,
      });
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };

    await appendAndDrain('first words ', 2);
    await appendAndDrain('paint directly ', 3);
    expect(firstMount.container.textContent).toContain('first words paint directly');

    firstMount.unmount();
    await tick();
    const remounted = render(TimelineLeaf, { props: { pane, item } });
    await tick();
    expect(remounted.container.textContent).toContain('first words paint directly');

    await appendAndDrain('after remount ', 4);
    expect(keeper.container.textContent).toContain(
      'first words paint directly after remount',
    );
    expect(remounted.container.textContent).toContain(
      'first words paint directly after remount',
    );
  });

  it('re-registers a mounted representation after a same-thread reload', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const thread = makeThread({ id: 'same-thread-reload' });
    const item = makeItem({
      id: 'same-thread-answer',
      threadId: thread.id,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(thread, [item], 'same-thread-reload-pane');
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const appendAndDrain = async (delta: string, updatedAt: number) => {
      pane.applyItemDelta({
        threadId: thread.id,
        itemId: item.id,
        kind: 'assistant_text',
        delta,
        updatedAt,
      });
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };

    await appendAndDrain('before reload ', 2);
    await appendAndDrain('paints directly ', 3);
    const reloadedItem = pane.getItemById(item.id);
    if (!reloadedItem) throw new Error('streaming item disappeared before reload');
    installThreadSwitchMocks(thread, [reloadedItem]);
    await pane.switchThread(thread);
    await tick();

    await appendAndDrain('after reload ', 4);
    await appendAndDrain('still direct ', 5);

    expect(view.container.textContent).toContain(
      'before reload paints directly after reload still direct',
    );
    expect(
      view.container.querySelector('[data-streamdown-direct-append-safe]')?.childNodes.length,
    ).toBeGreaterThan(1);
  });

  it('keeps the full direct suffix visible when a terminal patch settles mid-drain', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'terminal-direct-answer',
      threadId: 'terminal-direct-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'terminal-direct-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const drain = async () => {
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };
    pane.applyItemDelta({
      threadId: item.threadId,
      itemId: item.id,
      kind: 'assistant_text',
      delta: 'first words ',
      updatedAt: 2,
    });
    await drain();

    pane.applyItemDelta({
      threadId: item.threadId,
      itemId: item.id,
      kind: 'assistant_text',
      delta: 'paint directly ',
      updatedAt: 3,
    });
    pane.applyItemPatch({
      threadId: item.threadId,
      itemId: item.id,
      kind: 'assistant_text',
      patch: { status: 'completed', updatedAt: 4 },
    });
    await drain();

    expect(pane.getItemById(item.id)?.summary).toBe(
      'first words paint directly ',
    );
    expect(view.container.textContent).toContain('first words paint directly');
    for (const host of view.container.querySelectorAll(
      '[data-streamdown-direct-append-safe]',
    )) {
      expect(host.childNodes).toHaveLength(1);
    }
  });

  it('restores the full canonical tail after live updates are hidden and shown', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'toggle-streaming-answer',
      threadId: 'toggle-streaming-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'toggle-streaming-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const appendAndDrain = async (delta: string, updatedAt: number) => {
      pane.applyItemDelta({
        threadId: item.threadId,
        itemId: item.id,
        kind: 'assistant_text',
        delta,
        updatedAt,
      });
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };

    try {
      await appendAndDrain('first words ', 2);
      await appendAndDrain('paint directly ', 3);
      expect(view.container.textContent).toContain('first words paint directly');

      getSettings().streamingEnabled = false;
      await tick();
      getSettings().streamingEnabled = true;
      await tick();

      expect(view.container.textContent).toContain('first words paint directly');
    } finally {
      resetSettingsForTest();
    }
  });

  it('renders the full canonical tail before view-only linkification changes', async () => {
    setViewOnlySessionFromBootstrap(false);
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'view-only-streaming-answer',
      threadId: 'view-only-streaming-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: item.threadId }),
      [item],
      'view-only-streaming-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const appendAndDrain = async (delta: string, updatedAt: number) => {
      pane.applyItemDelta({
        threadId: item.threadId,
        itemId: item.id,
        kind: 'assistant_text',
        delta,
        updatedAt,
      });
      for (let frame = 0; clock.hasPendingFrame() && frame < 1_000; frame++) {
        clock.tickFrame(100);
        await tick();
      }
      if (clock.hasPendingFrame()) throw new Error('reveal did not drain');
    };

    await appendAndDrain('first words ', 2);
    await appendAndDrain('paint directly ', 3);
    const directHost = view.container.querySelector(
      '[data-streamdown-direct-append-safe]',
    );
    expect(directHost?.childNodes.length).toBeGreaterThan(1);

    setViewOnlySessionFromBootstrap(true);
    await tick();

    expect(view.container.textContent).toContain('first words paint directly');
    const authoritativeHost = view.container.querySelector(
      '[data-streamdown-direct-append-safe]',
    );
    expect(authoritativeHost?.childNodes).toHaveLength(1);
  });

  it('keeps the volatile markdown tree bounded across direct literal runs', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'streaming-answer',
      threadId: 'streaming-thread',
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      updatedAt: 1,
    });
    const pane = await buildPane(
      makeThread({ id: 'streaming-thread' }),
      [item],
      'streaming-pane',
    );
    const view = render(TimelineLeaf, { props: { pane, item } });
    await tick();

    const paragraphs = Array.from({ length: 24 }, (_, paragraph) =>
      `Paragraph ${paragraph} ${'ordinary streamed words '.repeat(24)}ends.`,
    );
    const source = paragraphs.join('\n\n');
    pane.applyItemDelta({
      threadId: 'streaming-thread',
      itemId: item.id,
      kind: 'assistant_text',
      delta: source,
      updatedAt: 2,
    });

    let maxVolatileCharacters = 0;
    for (let frame = 0; clock.hasPendingFrame() && frame < 10_000; frame++) {
      clock.tickFrame(100);
      await tick();
      maxVolatileCharacters = Math.max(
        maxVolatileCharacters,
        view.container.querySelector('.md-volatile')?.textContent?.length ?? 0,
      );
    }

    expect(clock.hasPendingFrame()).toBe(false);
    expect(pane.getItemById(item.id)?.summary).toBe(source);
    const body = view.getByTestId('assistant-message-body');
    const committed = body.querySelector('.md-committed');
    const volatile = body.querySelector('.md-volatile');
    expect(committed?.textContent).toContain(paragraphs.at(-2));
    expect(volatile?.textContent).toBe(paragraphs.at(-1));
    expect(maxVolatileCharacters).toBeLessThan(paragraphs[0].length * 2);
    expect(
      Array.from(body.querySelectorAll('p'), (paragraph) => paragraph.textContent),
    ).toEqual(paragraphs);
  });

  it('advances markdown boundaries in every pane when reveal frames are shared', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const paragraphs = Array.from({ length: 8 }, (_, paragraph) =>
      `Paragraph ${paragraph} ${'ordinary streamed words '.repeat(24)}ends.`,
    );
    const source = paragraphs.join('\n\n');
    const chunks = source.match(/\S+\s*|\s+/g) ?? [];
    const panes: Array<{
      item: ReturnType<typeof makeItem>;
      pane: Awaited<ReturnType<typeof buildPane>>;
      view: { container: HTMLElement };
    }> = [];
    for (let index = 0; index < 4; index++) {
      const threadId = `shared-frame-thread-${index}`;
      const item = makeItem({
        id: 'text:1:0',
        threadId,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: '',
        updatedAt: 1,
      });
      const pane = await buildPane(
        makeThread({ id: threadId }),
        [item],
        `shared-frame-pane-${index}`,
      );
      const view = render(TimelineLeaf, { props: { pane, item } });
      panes.push({ item, pane, view });
    }
    await tick();

    const maxVolatileCharacters = [0, 0, 0, 0];
    let chunkIndex = 0;
    let elapsed = 0;
    for (let frame = 0; chunkIndex < chunks.length || clock.hasPendingFrame(); frame++) {
      if (frame >= 10_000) throw new Error('shared-frame reveal did not drain');
      elapsed += 16;
      while (chunkIndex < chunks.length && chunkIndex * 45 <= elapsed) {
        const delta = chunks[chunkIndex];
        for (const { item, pane } of panes) {
          pane.applyItemDelta({
            threadId: item.threadId,
            itemId: item.id,
            kind: 'assistant_text',
            delta,
            updatedAt: chunkIndex + 2,
          });
        }
        chunkIndex++;
      }
      clock.tickFrame(16);
      await tick();
      for (let index = 0; index < panes.length; index++) {
        maxVolatileCharacters[index] = Math.max(
          maxVolatileCharacters[index],
          panes[index].view.container.querySelector('.md-volatile')?.textContent?.length ?? 0,
        );
      }
    }

    for (let index = 0; index < panes.length; index++) {
      const { item, pane, view } = panes[index];
      expect(pane.getItemById(item.id)?.summary).toBe(source);
      const body = view.container.querySelector('[data-testid="assistant-message-body"]');
      if (!body) throw new Error(`pane ${index} assistant body did not mount`);
      expect(
        Array.from(body.querySelectorAll('p'), (paragraph) => paragraph.textContent),
      ).toEqual(paragraphs);
      expect(body.querySelector('.md-volatile')?.textContent).toBe(paragraphs.at(-1));
      expect(maxVolatileCharacters[index]).toBeLessThan(paragraphs[0].length * 2);
    }
  });

});
