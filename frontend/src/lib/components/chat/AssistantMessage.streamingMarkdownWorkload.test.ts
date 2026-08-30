import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type { SmoothingClock } from '../../markdown/smoothing/PerItemSmoother';
import { __setSmoothingClockForTest } from '../../stores/thread.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { expectedStreamingFenceTexts } from '../../../test/helpers/streamingFenceOracle';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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

const ITERATIONS = 36;
const WIRE_INTERVAL_MS = 175;

function frameInterval(frame: number): number {
  // A long interval makes the smoother catch up several reveal units before
  // Svelte's next flush, which is the transition most likely to expose stale
  // parser or direct-DOM state. The ordinary 16ms step keeps this exhaustive
  // four-pane unit test bounded; the WebView benchmark covers the 165Hz clock.
  if (frame > 0 && frame % 375 === 0) return 110;
  return 16;
}

function iterationDeltas(iteration: number): string[] {
  return [
    `\n\n### Working set ${iteration}\n\n`,
    `The active pane keeps **streamed Markdown**, \`inline code\`, and [a link](https://example.test/active/${iteration}) readable. `,
    `Unicode remains intact: café, 東京, 🧪, and iteration ${iteration}.\n\n`,
    '- The parser carries state across wire chunks.\n- The reveal queue stays bounded.\n- The spring follows the live edge.\n\n',
    `| Iteration | Parser | Scroll |\n| ---: | :--- | :--- |\n| ${iteration} | active | following |\n\n`,
    '```ts\n',
    `const sample${iteration} = { pane: 1, active: true };\n`,
    `console.log(sample${iteration});\n\`\`\`\n\n`,
    `> Visible progress marker ${iteration}. The next section continues the same ordinary long turn.`,
  ];
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
});

beforeEach(() => {
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

describe('assistant streaming Markdown sustained workload', () => {
  it('never reopens a completed fence or skips the reveal cursor on terminal upsert', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const panes: Array<{
      item: ReturnType<typeof makeItem>;
      pane: Awaited<ReturnType<typeof buildPane>>;
      view: { container: HTMLElement };
    }> = [];
    for (let index = 0; index < 4; index++) {
      const threadId = `sustained-markdown-thread-${index}`;
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
        `sustained-markdown-pane-${index}`,
      );
      const view = render(TimelineLeaf, { props: { pane, item } });
      panes.push({ item, pane, view });
    }
    await tick();

    const wireDeltas = Array.from(
      { length: ITERATIONS },
      (_, iteration) => iterationDeltas(iteration),
    ).flat();
    const expectedSource = wireDeltas.join('');
    const priorTextLengths = panes.map(() => 0);
    const priorSources = panes.map(() => '');
    const maxDrops = panes.map(() => 0);
    const committedRoots: Array<HTMLElement | null> = panes.map(() => null);
    const committedPrefixes: Element[][] = panes.map(() => []);
    let wireIndex = 0;
    let elapsed = 0;
    let completionSent = false;

    for (let frame = 0; wireIndex < wireDeltas.length || clock.hasPendingFrame(); frame++) {
      if (frame >= 20_000) throw new Error('sustained Markdown reveal did not drain');
      const frameMs = frameInterval(frame);
      elapsed += frameMs;
      while (
        wireIndex < wireDeltas.length &&
        wireIndex * WIRE_INTERVAL_MS <= elapsed
      ) {
        const delta = wireDeltas[wireIndex];
        for (const { item, pane } of panes) {
          pane.applyItemDelta({
            threadId: item.threadId,
            itemId: item.id,
            kind: 'assistant_text',
            delta,
            updatedAt: wireIndex + 2,
          });
        }
        wireIndex++;
      }

      if (!completionSent && wireIndex === wireDeltas.length) {
        expect(panes.every(({ item, pane }) => pane.isItemSmoothing(item.id))).toBe(true);
        for (let index = 0; index < panes.length; index++) {
          const committed = panes[index].view.container.querySelector<HTMLElement>(
            '.md-committed',
          );
          committedRoots[index] = committed;
          committedPrefixes[index] = committed
            ? Array.from(committed.children)
            : [];
          const beforeCompletion = panes[index].pane.getItemById(
            panes[index].item.id,
          );
          if (!beforeCompletion) {
            throw new Error(`pane ${index} lost its assistant row at completion`);
          }
          const revealedSummary = beforeCompletion.summary;
          panes[index].pane.upsertItem({
            ...beforeCompletion,
            status: 'completed',
            summary: expectedSource,
            updatedAt: wireDeltas.length + 2,
          });
          expect(panes[index].pane.getItemById(panes[index].item.id)?.summary)
            .toBe(revealedSummary);
          expect(panes[index].pane.isItemSmoothing(panes[index].item.id)).toBe(true);
        }
        completionSent = true;
      }

      clock.tickFrame(frameMs);
      await tick();

      for (let index = 0; index < panes.length; index++) {
        const { item, pane, view } = panes[index];
        const current = pane.getItemById(item.id)?.summary ?? '';
        expect(expectedSource.startsWith(current)).toBe(true);
        expect(current.startsWith(priorSources[index])).toBe(true);
        priorSources[index] = current;
        const body = view.container.querySelector<HTMLElement>(
          '[data-testid="assistant-message-body"]',
        );
        if (!body) throw new Error(`pane ${index} assistant body did not mount`);

        const bodyText = body.textContent ?? '';

        const expectedCode = expectedStreamingFenceTexts(current);
        const renderedCode = Array.from(
          body.querySelectorAll('[data-code-source] code'),
          (code) => code.textContent ?? '',
        );
        if (expectedCode.hasOpenFence && renderedCode.length > 0) {
          renderedCode[renderedCode.length - 1] = renderedCode.at(-1)!.trimEnd();
        }
        expect(
          renderedCode,
          `pane ${index} frame ${frame} source length ${current.length}`,
        ).toEqual(expectedCode.texts);

        for (let marker = 0; marker < ITERATIONS; marker++) {
          const markerText = `Visible progress marker ${marker}.`;
          if (current.includes(markerText)) {
            expect(bodyText, `pane ${index} lost ${markerText} at frame ${frame}`)
              .toContain(markerText);
          }
        }
        const textLength = bodyText.length;
        maxDrops[index] = Math.max(maxDrops[index], priorTextLengths[index] - textLength);
        priorTextLengths[index] = textLength;
      }
    }

    for (let index = 0; index < panes.length; index++) {
      const { item, pane, view } = panes[index];
      expect(pane.getItemById(item.id)?.summary).toBe(expectedSource);
      expect(pane.getItemById(item.id)?.status).toBe('completed');
      expect(pane.isItemSmoothing(item.id)).toBe(false);
      expect(maxDrops[index]).toBeLessThan(128);
      const body = view.container.querySelector<HTMLElement>(
        '[data-testid="assistant-message-body"]',
      );
      expect(body?.querySelector('.md-volatile')).toBeNull();
      expect(body?.querySelector('.md-committed')).toBe(committedRoots[index]);
      const settledChildren = Array.from(
        body?.querySelector('.md-committed')?.children ?? [],
      );
      expect(settledChildren.slice(0, committedPrefixes[index].length)).toEqual(
        committedPrefixes[index],
      );
      expect(body?.querySelectorAll('[data-code-source]')).toHaveLength(ITERATIONS);
      for (let marker = 0; marker < ITERATIONS; marker++) {
        expect(body?.textContent).toContain(`Visible progress marker ${marker}.`);
      }
    }
    expect(completionSent).toBe(true);
  }, 60_000);
});
