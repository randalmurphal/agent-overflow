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
});
