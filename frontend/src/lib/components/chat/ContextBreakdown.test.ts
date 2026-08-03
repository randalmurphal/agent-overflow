import { describe, expect, it, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/svelte';

import ContextBreakdown from './ContextBreakdown.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

// Shape mirrors the Go `ThreadContextUsage` wire type; the fixture numbers
// are the real 2.1.219 capture in
// docs/references/fixtures/claude/context_usage_control_20260803.summary.json.
function fixture() {
  return {
    available: true,
    totalTokens: 24028,
    maxTokens: 1000000,
    percentage: 2,
    model: 'claude-fable-5',
    categories: [
      { name: 'System prompt', tokens: 4027 },
      { name: 'System tools', tokens: 15397 },
      { name: 'System tools (deferred)', tokens: 13467, deferred: true },
      { name: 'Custom agents', tokens: 105 },
      { name: 'Messages', tokens: 0 },
      { name: 'Free space', tokens: 942972 },
    ],
  };
}

describe('<ContextBreakdown>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('shows a loading line until the live read answers', async () => {
    let resolve: ((value: unknown) => void) | undefined;
    setBindingMock('GetThreadContextUsage', () => new Promise((r) => (resolve = r)));

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(screen.getByText('Reading exact usage…')).toBeTruthy();

    resolve?.(fixture());
    expect(await screen.findByText(/Exact: 24\.0k \/ 1\.0M \(2%\)/)).toBeTruthy();
    expect(screen.queryByText('Reading exact usage…')).toBeNull();
  });

  it('renders the totals and every non-empty category in wire order', async () => {
    setBindingMock('GetThreadContextUsage', () => Promise.resolve(fixture()));

    const { container } = render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText(/Exact: 24\.0k \/ 1\.0M \(2%\)/)).toBeTruthy();

    const rows = [...container.querySelectorAll('li')].map((li) =>
      li.textContent?.replace(/\s+/g, ' ').trim(),
    );
    expect(rows).toEqual([
      'System prompt 4.0k',
      'System tools 15.4k',
      'System tools (deferred) 13.5k',
      'Custom agents 105',
      'Free space 943.0k',
    ]);
    // The zero-token category carries no information and is dropped.
    expect(screen.queryByText('Messages')).toBeNull();
  });

  it('passes an unrecognised category straight through rather than dropping it', async () => {
    setBindingMock('GetThreadContextUsage', () =>
      Promise.resolve({
        available: true,
        totalTokens: 900,
        maxTokens: 200000,
        percentage: 1,
        categories: [{ name: 'Quantum ledger', tokens: 900 }],
      }),
    );

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText('Quantum ledger')).toBeTruthy();
  });

  it('explains deferred rows so the numbers not summing to the total reads as intentional', async () => {
    setBindingMock('GetThreadContextUsage', () => Promise.resolve(fixture()));

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText('Dimmed rows are not loaded into the prompt.')).toBeTruthy();
  });

  it('omits the deferred note when nothing is deferred', async () => {
    setBindingMock('GetThreadContextUsage', () =>
      Promise.resolve({
        available: true,
        totalTokens: 100,
        maxTokens: 200000,
        percentage: 0,
        categories: [{ name: 'System prompt', tokens: 100 }],
      }),
    );

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText('System prompt')).toBeTruthy();
    expect(screen.queryByText(/not loaded into the prompt/)).toBeNull();
  });

  // The unavailable answer is the whole reason the binding returns a typed
  // result instead of erroring: no live session must read as "start the
  // thread", never as an all-zero chart.
  it('states why the breakdown is unavailable instead of rendering zeros', async () => {
    setBindingMock('GetThreadContextUsage', () =>
      Promise.resolve({
        available: false,
        reason: 'The exact breakdown needs a running Claude session. Start the thread to read it.',
        totalTokens: 0,
        maxTokens: 0,
        percentage: 0,
        categories: [],
      }),
    );

    const { container } = render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(
      await screen.findByText(
        'The exact breakdown needs a running Claude session. Start the thread to read it.',
      ),
    ).toBeTruthy();
    expect(container.querySelectorAll('li')).toHaveLength(0);
    expect(screen.queryByText(/Exact:/)).toBeNull();
  });

  it('falls back to a generic sentence when the backend sends no reason', async () => {
    setBindingMock('GetThreadContextUsage', () =>
      Promise.resolve({ available: false, totalTokens: 0, maxTokens: 0, percentage: 0, categories: [] }),
    );

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText(/is not available right now/)).toBeTruthy();
  });

  // A provider fault is a distinct state from "no session" and must show the
  // CLI's own message rather than collapsing into the unavailable copy.
  it('surfaces a provider error verbatim', async () => {
    setBindingMock('GetThreadContextUsage', () =>
      Promise.reject(new Error('claude: get_context_usage: context analysis failed')),
    );

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });

    expect(await screen.findByText(/context analysis failed/)).toBeTruthy();
  });

  it('reads once per mount — it is user-initiated, never polled', async () => {
    const mock = setBindingMock('GetThreadContextUsage', () => Promise.resolve(fixture()));

    render(ContextBreakdown, { props: { threadId: 'thread-7' } });
    await screen.findByText(/Exact:/);

    expect(mock).toHaveBeenCalledTimes(1);
    expect(mock).toHaveBeenCalledWith('thread-7');
  });

  // Nothing is cached across popover opens: a second mount re-reads rather
  // than replaying a stale breakdown.
  it('re-reads on every mount', async () => {
    const mock = setBindingMock('GetThreadContextUsage', () => Promise.resolve(fixture()));

    const first = render(ContextBreakdown, { props: { threadId: 'thread-1' } });
    await screen.findByText(/Exact:/);
    first.unmount();

    render(ContextBreakdown, { props: { threadId: 'thread-1' } });
    await screen.findByText(/Exact:/);

    expect(mock).toHaveBeenCalledTimes(2);
  });

  // A response landing after the popover closed must not write into the
  // reopened popover's state.
  it('ignores a response that arrives after unmount', async () => {
    let resolve: ((value: unknown) => void) | undefined;
    setBindingMock('GetThreadContextUsage', () => new Promise((r) => (resolve = r)));

    const { unmount } = render(ContextBreakdown, { props: { threadId: 'thread-1' } });
    unmount();
    resolve?.(fixture());
    await Promise.resolve();

    expect(screen.queryByText(/Exact:/)).toBeNull();
  });
});
