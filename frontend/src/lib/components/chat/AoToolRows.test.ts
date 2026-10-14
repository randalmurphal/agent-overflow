// AO thread and workflow tool rows as a person reads them: the header names
// what the call did, the body decodes the reply into facts, links and
// folded text, a failed call shows its message and code, and a spawn
// follows its request until the answer lands.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import GenericToolCallRow from './GenericToolCallRow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../../test/helpers/chat';
import { resetStagedBackends } from '../../../test/helpers/backends';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { replaceAllThreads, resetThreadsForTest } from '../../stores/threads.svelte';
import { clearThreadStatus, projectSendResolved, projectSendStarted, setThreadStatus } from '../../stores/threadStatuses.svelte';
import { __resetItemProjectionSourceCacheForTest } from '../../utils/itemProjectionSource.svelte';
import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
import type { Item } from '../../types/models';

/** The pane surface the row reads: its expansion registry and its timeline. */
function paneWith(items: Item[]): never {
  const cache = new Map<string, ReturnType<typeof createPayloadExpansion>>();
  return {
    items,
    expansionStateFor(item: Item) {
      let handle = cache.get(item.id);
      if (!handle) {
        handle = createPayloadExpansion(() => item.payloadId, () => item.threadId);
        cache.set(item.id, handle);
      }
      return handle;
    },
  } as never;
}

const THREAD = 'thread-lead';
const CHILD = '7f2c9a41-aaaa-bbbb-cccc-dddddddddddd';
const TOKEN = '8613ba28-3563-47bd-b92a-8aa6f3f50ef5';

function aoItem(server: string, tool: string, meta: Record<string, unknown>, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: `tool-${tool}`,
    threadId: THREAD,
    kind: 'tool_call',
    status: 'completed',
    toolName: `MCP/${tool}`,
    summary: `MCP/${tool}: …`,
    meta: JSON.stringify({ mcp: { server, tool }, ...meta }),
    ...overrides,
  });
}

const thread = (tool: string, meta: Record<string, unknown>, overrides: Partial<Item> = {}) =>
  aoItem('ao-thread-tools', tool, meta, overrides);

function replyWith(data: string) {
  setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data, size: data.length, isComplete: true })));
}

const spawnAck = JSON.stringify({
  token: TOKEN, kind: 'spawn', thread_id: CHILD, title: 'Docs slice', state: 'running',
  outcome: 'backgrounded', revision: 0, notify: true, note: 'Still working. The answer will arrive here.',
});

describe('AO thread tool rows', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetItemProjectionSourceCacheForTest();
    resetThreadsForTest();
    clearThreadStatus(CHILD);
    projectSendResolved(CHILD);
    noteThread(THREAD, HOME_BACKEND);
    replyWith('');
  });

  it('heads a spawn with the new thread\'s title and opens to its reply as facts, never JSON', async () => {
    replyWith(spawnAck);
    const item = thread('thread_spawn', {
      input: { prompt: 'You own the docs slice.\nRead first: the guide.', worktree: 'docs/slice', model: 'opus' },
      ao_result: { token: TOKEN, thread_id: CHILD, title: 'Docs slice', state: 'running', outcome: 'backgrounded' },
    }, { payloadId: 'p-spawn' });
    const { getByTestId, findByTestId, getAllByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-label').textContent).toBe('spawn');
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('Docs slice');

    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    const outcome = await findByTestId('tool-call-card-ao-outcome');
    expect(outcome.textContent).toContain('running · backgrounded');
    const link = getByTestId('tool-call-card-ao-link');
    expect(link.textContent).toBe('Docs slice');
    expect(link.dataset.linkKind).toBe('thread');
    expect(link.tagName).toBe('BUTTON');
    const facts = getAllByTestId('tool-call-card-ao-result-fact').map((el) => [el.dataset.fact, el.textContent?.trim()]);
    expect(facts).toEqual([['branch', 'docs/slice'], ['model', 'opus'], ['notify', 'armed'], ['request', '8613ba28']]);
    expect(getByTestId('tool-call-card-ao-note').textContent).toContain('Still working');
    expect(getByTestId('tool-call-card-output').textContent).not.toContain('"token"');
  });

  it('reads a clipped prompt from the stored row when the body opens', async () => {
    const full = `You own the docs slice.\n${'Context line.\n'.repeat(200)}`;
    const source = vi.fn(async () => ({ meta: JSON.stringify({ mcp: { server: 'ao-thread-tools', tool: 'thread_spawn' }, input: { prompt: full } }), payloadMeta: '', payloadPreviewSpans: '' }));
    setBindingMock('GetThreadItemProjectionSource', source);
    const item = thread('thread_spawn', {
      input: { prompt: `${full.slice(0, 512)}…` },
      wireElision: { input: ['prompt'] },
    });
    const { getByTestId, findAllByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('You own the docs slice.');
    expect(source).not.toHaveBeenCalled();
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await waitFor(() => expect(source).toHaveBeenCalledWith(THREAD, item.id));
    const [prompt] = await findAllByTestId('tool-call-card-ao-fact-text');
    expect(prompt.dataset.folded).toBe('true');
    const toggle = getByTestId('tool-call-card-ao-fact-text-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await fireEvent.click(toggle);
    await waitFor(() => expect(prompt.textContent?.length ?? 0).toBeGreaterThan(2000));
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
  });

  it('follows the target thread until the answer\'s wake row lands', async () => {
    replaceAllThreads([makeThread({ id: CHILD, title: 'Docs slice' })]);
    projectSendStarted(CHILD);
    const item = thread('thread_spawn', {
      input: { prompt: 'p' },
      ao_result: { token: TOKEN, thread_id: CHILD, title: 'Docs slice', state: 'running', outcome: 'backgrounded' },
    });
    const view = render(GenericToolCallRow, { props: { item, pane: paneWith([item]) } });
    expect(view.getByTestId('tool-call-card-ao-live-hint').textContent).toBe('(thread running)');

    setThreadStatus(CHILD, 'pending-approval');
    await waitFor(() => expect(view.getByTestId('tool-call-card-ao-live-hint').textContent).toBe('(waiting on an approval)'));

    const wake = makeItem({
      id: 'wake-1', threadId: THREAD, kind: 'user_text',
      meta: JSON.stringify({ origin: 'agent-thread', originThread: { threadId: CHILD, title: 'Docs slice', token: TOKEN } }),
    });
    await view.rerender({ item, pane: paneWith([item, wake]) });
    expect(view.getByTestId('tool-call-card-ao-live-hint').textContent).toBe('(answer arrived)');
  });

  it('finds its answer in a row that joined answers which queued together', () => {
    replaceAllThreads([makeThread({ id: CHILD, title: 'Docs slice' })]);
    const item = thread('thread_send', {
      input: { thread_id: CHILD, message: 'm' },
      ao_result: { token: TOKEN, thread_id: CHILD, title: 'Docs slice', state: 'queued', outcome: 'queued' },
    });
    // The chip keeps the first member's thread; this request's token is the second.
    const joined = makeItem({
      id: 'wake-1', threadId: THREAD, kind: 'user_text',
      meta: JSON.stringify({
        origin: 'agent-thread',
        originThread: { threadId: 'other', title: 'Other', token: 'first-token' },
        joinedOriginTokens: ['first-token', TOKEN],
      }),
    });
    const view = render(GenericToolCallRow, { props: { item, pane: paneWith([item, joined]) } });
    expect(view.getByTestId('tool-call-card-ao-live-hint').textContent).toBe('(answer arrived)');

    // A wake joined first: the row is the wake's, and still answers the request.
    const behindWake = makeItem({
      id: 'wake-2', threadId: THREAD, kind: 'user_text',
      meta: JSON.stringify({ origin: 'agent-wake', originWake: { kind: 'loop', label: 'board' }, joinedOriginTokens: [TOKEN] }),
    });
    const other = render(GenericToolCallRow, { props: { item, pane: paneWith([item, behindWake]) } });
    expect(other.getAllByTestId('tool-call-card-ao-live-hint').at(-1)!.textContent).toBe('(answer arrived)');
  });

  it('drops the reply\'s "still working" note once the answer has landed', async () => {
    replyWith(spawnAck);
    const item = thread('thread_spawn', {
      input: { prompt: 'p' },
      ao_result: { token: TOKEN, thread_id: CHILD, title: 'Docs slice', state: 'running', outcome: 'backgrounded' },
    }, { payloadId: 'p-spawn' });
    const wake = makeItem({
      id: 'wake-1', threadId: THREAD, kind: 'user_text',
      meta: JSON.stringify({ origin: 'agent-thread', originThread: { threadId: CHILD, title: 'Docs slice', token: TOKEN } }),
    });
    const { getByTestId, findByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item, pane: paneWith([item, wake]) } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-outcome')).textContent).toContain('running · backgrounded → answer arrived');
    expect(queryByTestId('tool-call-card-ao-note')).toBeNull();
  });

  it('stops following once the call carried the answer', () => {
    replaceAllThreads([makeThread({ id: CHILD, title: 'Docs slice' })]);
    projectSendStarted(CHILD);
    const item = thread('thread_send', {
      input: { thread_id: CHILD, message: 'go' },
      ao_result: { token: TOKEN, thread_id: CHILD, title: 'Docs slice', state: 'finished', outcome: 'settled' },
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item, pane: paneWith([item]) } });
    expect(queryByTestId('tool-call-card-ao-live-hint')).toBeNull();
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('Docs slice · go');
  });

  it('shows a failed call\'s message with its code, not the raw text', async () => {
    replyWith('[thread_not_found] No thread matches "abc123". Find the thread with thread_search.');
    const item = thread('thread_show', { input: { thread_id: 'abc123' }, is_error: true }, {
      status: 'errored',
      payloadId: 'p-err',
      payloadMeta: JSON.stringify({ isError: true, preview: '[thread_not_found] No thread matches "abc123". Find the thread with thread_search.' }),
    });
    const { getAllByTestId, getByTestId, findByTestId, container } = render(GenericToolCallRow, { props: { item } });
    const [header] = getAllByTestId('row-error-code');
    expect(header.textContent).toBe('thread_not_found');
    expect(container.textContent).toContain('No thread matches "abc123". Find the thread with thread_search.');
    expect(container.textContent).not.toContain('[thread_not_found]');
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('thread abc123 · last 20 turns');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    // The header already says it whole; the body does not repeat it.
    expect((await findByTestId('tool-call-card-ao-error')).textContent).toContain('shown above');
  });

  it('shows a long message whole in the header once the body has read it, and never twice', async () => {
    const message = `Branch "feature/x" is checked out elsewhere. ${'More detail. '.repeat(40)}The end of it.`;
    replyWith(`[thread_invalid_request] ${message}`);
    const item = thread('thread_spawn', { input: { prompt: 'p', worktree: 'feature/x' }, is_error: true }, {
      status: 'errored',
      payloadId: 'p-err',
      payloadMeta: JSON.stringify({ isError: true, preview: `[thread_invalid_request] ${message.slice(0, 214)}…` }),
    });
    const { getAllByTestId, getByTestId, findByTestId, container } = render(GenericToolCallRow, { props: { item } });
    expect(getAllByTestId('row-error-msg')[0].textContent).not.toContain('The end of it.');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-error')).textContent).toContain('shown above');
    const [header] = getAllByTestId('row-error-msg');
    expect(header.textContent).toBe(message);
    expect(getAllByTestId('row-error-code')).toHaveLength(1);
    expect(container.textContent!.split('The end of it.').length - 1).toBe(1);
  });

  it('lists a status call\'s requests and folds past the list limit', async () => {
    const requests = Array.from({ length: 11 }, (_, i) => ({ token: `t${i}`, kind: 'send', thread_id: `th-${i}`, title: `Worker ${i}`, state: i === 0 ? 'finished' : 'running', answer_kind: i === 0 ? 'final' : '', answer: i === 0 ? 'Shipped it.' : '' }));
    replyWith(JSON.stringify({ requests }));
    const item = thread('thread_status', { input: {} }, { payloadId: 'p-status' });
    const { getByTestId, findAllByTestId, getAllByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('all requests');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    const lines = await findAllByTestId('tool-call-card-ao-line');
    expect(lines).toHaveLength(8);
    expect(lines[0].textContent).toContain('Worker 0');
    expect(lines[0].textContent).toContain('finished · final');
    expect(lines[0].textContent).toContain('Shipped it.');
    const more = getByTestId('tool-call-card-ao-lines-toggle');
    expect(more.textContent).toBe('Show all 11');
    await fireEvent.click(more);
    expect(getAllByTestId('tool-call-card-ao-line')).toHaveLength(11);
    expect(more).toHaveAttribute('aria-expanded', 'true');
  });

  it('heads a read with the thread and window and folds its transcript', async () => {
    replaceAllThreads([makeThread({ id: CHILD, title: 'Docs slice' })]);
    const transcript = Array.from({ length: 30 }, (_, i) => `[user] line ${i}`).join('\n');
    replyWith(JSON.stringify({ thread_id: CHILD, title: 'Docs slice', state: 'idle', window: 'last_answer', items: 1, bytes: 2048, done: true, transcript }));
    const item = thread('thread_show', { input: { thread_id: CHILD.slice(0, 8), window: 'last_answer' } }, { payloadId: 'p-show' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('Docs slice · last answer');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-outcome')).textContent).toContain('last answer · 1 item · 2.0 KB');
    const preview = getByTestId('tool-call-card-ao-preview');
    expect(preview.dataset.folded).toBe('true');
    expect(preview.textContent).toContain('line 5');
    expect(preview.textContent).not.toContain('line 29');
  });
});

describe('AO tool replies past the payload preview, and replies nothing decodes', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetItemProjectionSourceCacheForTest();
    resetThreadsForTest();
    noteThread(THREAD, HOME_BACKEND);
    replaceAllThreads([makeThread({ id: CHILD, title: 'Docs slice' })]);
  });

  /** A reply served as a 32 KB preview and then in chunks, the way the store serves one. */
  function serveInChunks(full: string, preview = 32 * 1024) {
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const chunk = vi.fn(async (_thread: string, _id: string, offset: number, length: number) => {
      await gate;
      const data = full.slice(offset, offset + length);
      return { data, nextOffset: offset + data.length, totalSize: full.length, isComplete: offset + data.length >= full.length };
    });
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({
      data: full.slice(0, preview), nextOffset: Math.min(preview, full.length), totalSize: full.length, isComplete: full.length <= preview,
    })));
    setBindingMock('GetPayloadChunk', chunk);
    return { chunk, release };
  }

  it('reads a 65 KB thread_show reply whole before decoding it, and never paints the JSON', async () => {
    const transcript = Array.from({ length: 2000 }, (_, i) => `[assistant] line ${i} of the answer`).join('\n');
    const full = JSON.stringify({ thread_id: CHILD, title: 'Docs slice', state: 'idle', window: 'last_turns', items: 40, bytes: transcript.length, done: true, transcript });
    expect(full.length).toBeGreaterThan(65 * 1024);
    const { chunk, release } = serveInChunks(full);
    const item = thread('thread_show', { input: { thread_id: CHILD } }, { payloadId: 'p-big' });
    const { getByTestId, findByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    // Until the rest arrives the body says what it is doing, not the JSON.
    await waitFor(() => expect(chunk).toHaveBeenCalled());
    const output = getByTestId('tool-call-card-body');
    expect(getByTestId('tool-call-card-ao-undecoded').textContent).toMatch(/^Reading the whole reply \(\d+(\.\d)? KB\)…$/);
    expect(output.textContent).not.toContain('Loading…');
    expect(output.textContent).not.toContain('"transcript"');
    expect(queryByTestId('tool-call-card-show-full')).toBeNull();
    release();
    expect((await findByTestId('tool-call-card-ao-outcome')).textContent).toContain('last turns · 40 items');
    expect(getByTestId('tool-call-card-ao-link').textContent).toBe('Docs slice');
    expect(getByTestId('tool-call-card-ao-preview').textContent).toContain('line 0 of the answer');
    expect(output.textContent).not.toContain('"transcript"');
  });

  it('stops reading a reply whose next chunk failed, and offers a retry', async () => {
    const full = JSON.stringify({ thread_id: CHILD, title: 'Docs slice', transcript: 'x'.repeat(40 * 1024) });
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: full.slice(0, 32 * 1024), nextOffset: 32 * 1024, totalSize: full.length, isComplete: false })));
    // The read fails a timer later, so a retry loop runs at the pace of
    // the test's own wait rather than starving it.
    const chunk = vi.fn(() => new Promise<never>((_, reject) => setTimeout(() => reject(new Error('chunk read failed')), 1)));
    setBindingMock('GetPayloadChunk', chunk);
    const item = thread('thread_show', { input: { thread_id: CHILD } }, { payloadId: 'p-chunk-fails' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await findByTestId('tool-call-card-retry');
    expect(getByTestId('tool-call-card-body').textContent).toContain('Failed to load: chunk read failed');
    // Give a retry loop the time it would need to show itself.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(chunk).toHaveBeenCalledTimes(1);
    expect(getByTestId('tool-call-card-retry')).toBeTruthy();
  });

  it('says a reply past the read limit is too large instead of loading or painting it', async () => {
    const full = `{"transcript":"${'x'.repeat(40 * 1024)}`;
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: full.slice(0, 32 * 1024), nextOffset: 32 * 1024, totalSize: 3 * 1024 * 1024, isComplete: false })));
    const chunk = vi.fn();
    setBindingMock('GetPayloadChunk', chunk);
    const item = thread('thread_show', { input: { thread_id: CHILD } }, { payloadId: 'p-huge' });
    const { getByTestId, findByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-undecoded')).textContent).toContain('Reply of 3.0 MB, too large to read here.');
    expect(getByTestId('tool-call-card-body').textContent).not.toContain('"transcript"');
    expect(chunk).not.toHaveBeenCalled();
    expect(queryByTestId('tool-call-card-show-full')).toBeNull();
    const raw = getByTestId('tool-call-card-ao-raw-toggle');
    await fireEvent.click(raw);
    expect(raw).toHaveAttribute('aria-expanded', 'true');
    expect(getByTestId('tool-call-card-body').textContent).toContain('"transcript"');
    expect(queryByTestId('tool-call-card-show-full')).not.toBeNull();
  });

  it('says a reply no decoder reads was not decoded, and shows it only on request', async () => {
    replyWith('Done, nothing to report.');
    const item = thread('thread_show', { input: { thread_id: CHILD } }, { payloadId: 'p-text' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-undecoded')).textContent).toContain('Reply not decoded, 24 B.');
    expect(getByTestId('tool-call-card-body').textContent).not.toContain('nothing to report');
    await fireEvent.click(getByTestId('tool-call-card-ao-raw-toggle'));
    expect(getByTestId('tool-call-card-body').textContent).toContain('Done, nothing to report.');
  });

  it('reads a list reply as lines', async () => {
    replyWith(JSON.stringify([{ token: 't0000000', state: 'running' }]));
    const item = thread('thread_status', { input: { tokens: ['t0000000'] } }, { payloadId: 'p-list' });
    const { getByTestId, findAllByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    const lines = await findAllByTestId('tool-call-card-ao-line');
    expect(lines[0].textContent).toContain('request t0000000');
    expect(getByTestId('tool-call-card-body').textContent).not.toContain('"token"');
  });

  it('lists inputs whose labels collide as separate facts', async () => {
    replyWith('');
    // The each-key repair reports a duplicate key here instead of throwing.
    const reported = vi.fn();
    vi.stubGlobal('reportError', reported);
    const item = thread('thread_send', { input: { thread_id: CHILD, message: 'm', extra_key: 'one', 'extra key': 'two' } });
    const view = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(view.getByTestId('tool-call-card-toggle'));
    const read = () => view.getAllByTestId('tool-call-card-ao-fact')
      .filter((el) => el.dataset.fact === 'extra key')
      .map((el) => el.textContent?.trim());
    expect(read()).toEqual(['one', 'two']);
    // The same row re-read keeps one fact per input.
    await view.rerender({ item: { ...item, meta: item.meta!.replace('"one"', '"three"').replace('"two"', '"four"') } });
    expect(read()).toEqual(['three', 'four']);
    expect(reported).not.toHaveBeenCalled();
  });

  it('titles a request with no thread by its kind, with its detail after a space', async () => {
    replyWith(JSON.stringify({ requests: [{ token: 'r1', kind: 'remind', state: 'accepted', answer_kind: 'note' }] }));
    const item = thread('thread_status', { input: {} }, { payloadId: 'p-remind' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-ao-line-title')).textContent).toBe('remind · accepted · note');
  });

  it('lists a fact the input and the reply both state once, as the reply says it', async () => {
    replyWith(JSON.stringify({ thread_id: CHILD, effect: 'cancelled', state: 'cancelled', label: 'nightly board', token: TOKEN }));
    const item = thread('thread_cancel', { input: { thread_id: CHILD, label: 'nightly board', token: TOKEN } }, { payloadId: 'p-cancel' });
    const { getByTestId, findByTestId, container } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await findByTestId('tool-call-card-ao-outcome');
    const labels = [...container.querySelectorAll('[data-fact]')].map((el) => [el.getAttribute('data-testid'), el.getAttribute('data-fact')]);
    expect(labels.filter(([, label]) => label === 'label')).toEqual([['tool-call-card-ao-result-fact', 'label']]);
    expect(labels.filter(([, label]) => label === 'request')).toEqual([['tool-call-card-ao-result-fact', 'request']]);
  });

  it('heads and links an ask by the thread asked, not the copy that answers', async () => {
    replyWith(JSON.stringify({ token: TOKEN, kind: 'ask', thread_id: 'fork-9', title: 'Copy of Docs slice', state: 'running', outcome: 'backgrounded', revision: 0, notify: true }));
    const item = thread('thread_ask', {
      input: { thread_id: CHILD, question: 'Why did the build fail?' },
      ao_result: { token: TOKEN, thread_id: 'fork-9', title: 'Copy of Docs slice', state: 'running', outcome: 'backgrounded' },
    }, { payloadId: 'p-ask' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item, pane: paneWith([item]) } });
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('Docs slice · Why did the build fail?(waiting on the answer)');
    expect(getByTestId('tool-call-card-ao-live-hint').textContent).toBe('(waiting on the answer)');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await findByTestId('tool-call-card-ao-outcome');
    const link = getByTestId('tool-call-card-ao-link');
    expect(link.textContent).toBe('Docs slice');
    expect(link.getAttribute('title')).toBe('Open Docs slice');
  });

  it('links an ask addressed by an id prefix to the full id, and leaves an ambiguous prefix as text', async () => {
    const target = 'target-thr-aaaa-bbbb-cccc-dddddddddd';
    replyWith(JSON.stringify({ token: TOKEN, kind: 'ask', thread_id: 'fork-9', state: 'running', outcome: 'backgrounded', revision: 0, notify: true }));
    replaceAllThreads([makeThread({ id: target, title: 'Release notes' })]);
    const item = thread('thread_ask', {
      input: { thread_id: 'target-thr', question: 'What shipped?' },
      ao_result: { token: TOKEN, thread_id: 'fork-9', state: 'running', outcome: 'backgrounded' },
    }, { payloadId: 'p-ask-prefix' });
    const view = render(GenericToolCallRow, { props: { item, pane: paneWith([item]) } });
    expect(view.getByTestId('tool-call-card-preview').textContent).toContain('Release notes · What shipped?');
    await fireEvent.click(view.getByTestId('tool-call-card-toggle'));
    await view.findByTestId('tool-call-card-ao-outcome');
    const link = view.getByTestId('tool-call-card-ao-link');
    expect(link.dataset.linkId).toBe(target);
    expect(link.tagName).toBe('BUTTON');
    view.unmount();

    // Two threads share the prefix: no one thread is named, so no link.
    replaceAllThreads([makeThread({ id: target, title: 'Release notes' }), makeThread({ id: 'target-thr-eeee-bbbb-cccc-dddddddddd', title: 'Other' })]);
    const ambiguous = render(GenericToolCallRow, { props: { item, pane: paneWith([item]) } });
    expect(ambiguous.getByTestId('tool-call-card-preview').textContent).toContain('thread target-thr · What shipped?');
    await fireEvent.click(ambiguous.getByTestId('tool-call-card-toggle'));
    await ambiguous.findByTestId('tool-call-card-ao-outcome');
    expect(ambiguous.queryByTestId('tool-call-card-ao-link')).toBeNull();
    const shown = ambiguous.getAllByTestId('tool-call-card-ao-result-fact').map((el) => [el.dataset.fact, el.textContent?.trim()]);
    expect(shown).toContainEqual(['thread', 'target-thr']);
  });
});

describe('AO workflow tool rows', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetEntityIndexForTest();
    noteThread(THREAD, HOME_BACKEND);
    replyWith('');
  });

  it('heads a run status with its workflow and state and links the run', async () => {
    const run = '1a2b3c4d-aaaa-bbbb-cccc-dddddddddddd';
    replyWith(JSON.stringify({
      run: { itemId: run, workflowId: 'digest', goal: 'Weekly summary', state: 'running', currentPhaseId: 'collect', currentPhaseOrdinal: 1, phaseCount: 2 },
      outcome: 'backgrounded',
      notify: { armed: true, detail: 'its rest reaches this thread' },
    }));
    const item = aoItem('ao-workflow-tools', 'run_status', {
      input: { run_id: run, wait_seconds: 30 },
      ao_result: { run_id: run, workflow_id: 'digest', state: 'running', outcome: 'backgrounded' },
    }, { payloadId: 'p-run' });
    const { getByTestId, findByTestId, getAllByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-preview').textContent?.trim()).toBe('digest run 1a2b3c4d · running');
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await findByTestId('tool-call-card-ao-outcome');
    const link = getByTestId('tool-call-card-ao-link');
    expect(link.dataset.linkKind).toBe('run');
    expect(link.textContent).toBe('digest · Weekly summary');
    const facts = getAllByTestId('tool-call-card-ao-result-fact').map((el) => [el.dataset.fact, el.textContent?.trim()]);
    expect(facts).toEqual([['phase', 'collect · 1 of 2'], ['notify', 'armed · its rest reaches this thread']]);
  });
});
