import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import MessageTimeline from './MessageTimeline.svelte';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

describe('<MessageTimeline>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('renders the empty state for a blank thread', async () => {
    const pane = await buildPane();
    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/No messages yet/i)).toBeInTheDocument();
  });

  it('renders user, assistant, error, and compaction rows from unified items', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, kind: 'assistant_text', summary: 'hello' }),
      makeItem({ id: 'error:0:0', itemIndex: 2, kind: 'error', role: 'system', summary: 'boom' }),
      makeItem({ id: 'compact:1', turnIndex: 1, itemIndex: 0, kind: 'compaction', role: 'system', summary: 'Context compacted' }),
    ]);

    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText('hi')).toBeInTheDocument();
    expect(getByText('hello')).toBeInTheDocument();
    expect(getByText('boom')).toBeInTheDocument();
    expect(getByText('Context compacted')).toBeInTheDocument();
  });

  it('renders changed-files and turn-diff summaries from tool-result payloads', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        summary: 'Edit src/a.ts',
        payloadId: 'payload-1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          title: 'File change',
          inlineDiff: {
            availability: 'exact_patch',
            files: [
              { path: 'src/a.ts', kind: 'modified', insertions: 5, deletions: 2 },
              { path: 'src/b.ts', kind: 'added', insertions: 3, deletions: 0 },
            ],
          },
        }),
      }),
    ]);

    const { getByText, getByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/2 files changed/i)).toBeInTheDocument();
    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+8');
    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('−2');
  });

  it('renders proposed plans from payload-bearing tool rows', async () => {
    setBindingMock('GetPayloadData', async () => '# Ship it');
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        summary: 'Plan',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Ship it',
          lineCount: 3,
          charCount: 12,
          preview: '# Ship it',
        }),
      }),
    ]);

    const { getAllByText } = render(MessageTimeline, { props: { pane } });

    expect(getAllByText('Ship it').length).toBeGreaterThan(0);
  });

  it('wraps each root node in a content-visibility container for off-screen skipping', async () => {
    const items = Array.from({ length: 50 }, (_, i) =>
      makeItem({
        id: `text:${i}`,
        turnIndex: Math.floor(i / 10),
        itemIndex: i % 10,
        summary: `message ${i}`,
        createdAt: i,
      }),
    );
    const pane = await buildPane(undefined, items);
    const { container } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    // One wrapper per root timeline node. With no subagent grouping,
    // that's one wrapper per item.
    expect(wrappers.length).toBe(50);
    // Every wrapper applies the CSS class that opts into
    // content-visibility: auto. We assert on the class rather than the
    // computed style because happy-dom doesn't implement the property.
    for (const w of wrappers) {
      expect(w.classList.contains('contents-visibility-auto')).toBe(true);
    }
  });

  it('rebuilds turn summaries incrementally via the pane (not per-upsert full scan)', async () => {
    // Regression pin for the task-2 refactor: MessageTimeline must source
    // turnDiffViews from the pane (pane.turnDiffViews) rather than a
    // component-local $derived that rescans pane.items on every upsert.
    // This test injects an item, then upserts a second diff into the same
    // turn, and checks that both contributions land in the rendered badge.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'src/a.ts',
          changeKind: 'modified',
          insertions: 3,
          deletions: 1,
          preview: '',
        }),
      }),
    ]);
    const { getByTestId, rerender } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+3');

    pane.upsertItem(makeItem({
      id: 'tool-2',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_call',
      payloadId: 'payload-2',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'src/b.ts',
        changeKind: 'added',
        insertions: 2,
        deletions: 0,
        preview: '',
      }),
    }));
    await rerender({ pane });

    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+5');
  });
});
