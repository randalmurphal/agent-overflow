// An expanded reasoning row's payload keeps the live stream its reveals
// arrive on (utils/payloadExpansion.svelte.ts). The stream reads the
// smoother's text, and the expansion outlives the smoother, so the stream
// must let go of that text once the smoother is disposed.
import { beforeEach, expect, it } from 'vitest';
import { __setSmoothingClockForTest } from './thread.svelte';
import { getSettings } from './settings.svelte';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { FakeSmoothingClock, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { LiveRevealStream } from '../utils/payloadExpansion.svelte';
import {
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  thinkingPayloadVersionForItem,
} from '../utils/payloadVersion';

beforeEach(installThreadPaneTestEnv);

it.each([
  ['settles', { status: 'completed' }],
  ['errors', { status: 'errored' }],
  ['leaves with its thread', null],
] as const)('releases the smoother text from the live stream when the row %s', async (_name, patch) => {
  const clock = new FakeSmoothingClock();
  __setSmoothingClockForTest(clock);
  // Low power reveals each wire chunk whole.
  getSettings().lowPowerMode = true;
  try {
    setBindingMock('GetPayloadData', async () => ({ data: '' }));
    const pane = await buildPane(makeThread({ id: 'thread-stream' }));
    const id = 'think:0:0';
    const row = makeItem({
      id,
      threadId: 'thread-stream',
      kind: 'thinking',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      payloadId: `thinking:${id}`,
      updatedAt: 1,
    });
    pane.upsertItem(row);
    const expansion = pane.expansionStateFor(row, {
      loadMode: 'full',
      stateKey: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      payloadVersion: thinkingPayloadVersionForItem,
    });
    await expansion.expand();
    const streams: LiveRevealStream[] = [];
    const appendLiveDelta = expansion.appendLiveDelta;
    expansion.appendLiveDelta = (stream, ...rest) => {
      streams.push(stream);
      appendLiveDelta(stream, ...rest);
    };

    pane.applyItemDelta({
      threadId: 'thread-stream',
      itemId: id,
      kind: 'thinking',
      delta: 'weighing the options\n',
      updatedAt: 2,
    });
    clock.tickFrame(16);
    expect(expansion.displayData).toBe('weighing the options\n');
    const [stream] = streams;
    expect(stream.revealedText(8)).toBe('weighing');

    if (patch) {
      pane.applyItemPatch({ threadId: 'thread-stream', itemId: id, kind: 'thinking', patch: { rev: 0, ...patch, updatedAt: 3 } });
    } else {
      await pane.switchThread(makeThread({ id: 'thread-next' }));
    }
    expect(pane.__itemSmootherCountForTest()).toBe(0);
    expect(stream.revealedText(8)).toBeNull();
  } finally {
    getSettings().lowPowerMode = false;
    __setSmoothingClockForTest(undefined);
  }
});
