import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ChannelView from './ChannelView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { ChannelMessage } from '../../types/discussion';
import type { Thread } from '../../types/models';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

// Stub Element.animate for Svelte transitions (reused by Markdown's nested components).
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function makeThread(): Thread {
  return {
    id: 'parent-thread',
    title: 'Deliberation',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'discussion',
    discussionId: 'channel-1',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function makeMsg(overrides: Partial<ChannelMessage> = {}): ChannelMessage {
  return {
    id: 'm' + (overrides.sequence ?? 0),
    channelId: 'channel-1',
    sequence: 1,
    fromType: 'agent',
    fromId: 'agent-id',
    fromRole: 'advocate',
    content: 'hello',
    highlightedContent: '',
    createdAt: 0,
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ChannelView>', () => {
  beforeEach(async () => {
    vi.useFakeTimers();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('loads initial channel messages via GetChannelMessages and renders them', async () => {
    const pane = await buildPane();
    const getMock = setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 1, content: 'first', fromRole: 'advocate' }),
      makeMsg({ id: 'b', sequence: 2, content: 'second', fromRole: 'critic' }),
    ]);
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    expect(await findByText('first')).toBeInTheDocument();
    expect(await findByText('second')).toBeInTheDocument();
    // First call uses afterSeq = 0.
    expect(getMock.mock.calls[0]).toEqual(['channel-1', 0, 200]);
    expect(pane.channelMessages.length).toBe(2);
  });

  it('polls incrementally with the highest seen sequence as afterSeq', async () => {
    const pane = await buildPane();
    let callCount = 0;
    const getMock = setBindingMock('GetChannelMessages', async () => {
      callCount++;
      if (callCount === 1) {
        return [makeMsg({ id: 'a', sequence: 1 })];
      }
      return [makeMsg({ id: 'b', sequence: 2, content: 'second' })];
    });
    render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    // initial call
    await vi.advanceTimersByTimeAsync(0);
    await Promise.resolve();
    expect(getMock.mock.calls[0][1]).toBe(0);
    // poll timer fires
    await vi.advanceTimersByTimeAsync(2500);
    // settle any pending awaits
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(getMock.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(getMock.mock.calls[1][1]).toBe(1);
    expect(pane.channelMessages.length).toBe(2);
  });

  it('posts user messages via PostChannelMessage and immediately re-polls', async () => {
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [makeMsg({ id: 'a', sequence: 1 })]);
    const postMock = setBindingMock('PostChannelMessage', async () => {});
    const { container, getByRole } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel message input"]')!;
    await fireEvent.input(textarea, { target: { value: 'my intervention' } });
    const sendBtn = getByRole('button', { name: /post/i }) as HTMLButtonElement;
    await fireEvent.click(sendBtn);
    // Allow the async post to complete.
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(postMock.mock.calls[0]).toEqual(['channel-1', 'my intervention']);
    expect(textarea.value).toBe('');
  });

  it('disables posting and shows Concluded badge when channel status is concluded', async () => {
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => []);
    const { getAllByText, queryByRole } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    // Simulate conclusion arriving (deliberation engine hit max turns).
    pane.setChannelStatus('concluded');
    for (let i = 0; i < 3; i++) await Promise.resolve();
    const concludedMatches = getAllByText(/concluded/i);
    expect(concludedMatches.length).toBeGreaterThanOrEqual(1);
    // Composer is hidden when concluded, so there is no Post button.
    expect(queryByRole('button', { name: /post/i })).toBeNull();
  });

  it('rolls back the textarea when PostChannelMessage rejects', async () => {
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => []);
    setBindingMock('PostChannelMessage', async () => { throw new Error('rpc-down'); });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { container, getByRole } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel message input"]')!;
    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByRole('button', { name: /post/i }));
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(textarea.value).toBe('fails');
    consoleErr.mockRestore();
  });

  it('merges incremental messages without duplicating by sequence', async () => {
    const pane = await buildPane();
    let callCount = 0;
    setBindingMock('GetChannelMessages', async () => {
      callCount++;
      if (callCount === 1) {
        return [
          makeMsg({ id: 'a', sequence: 1 }),
          makeMsg({ id: 'b', sequence: 2 }),
        ];
      }
      // Server repeats seq=2 (simulating overlap) plus adds seq=3.
      return [
        makeMsg({ id: 'b', sequence: 2 }),
        makeMsg({ id: 'c', sequence: 3 }),
      ];
    });
    render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(pane.channelMessages.length).toBe(2);
    await vi.advanceTimersByTimeAsync(2500);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(pane.channelMessages.length).toBe(3);
    expect(pane.channelMessages.map((m) => m.sequence)).toEqual([1, 2, 3]);
  });
});
