import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ChannelView from './ChannelView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { ChannelMessage } from '../../types/discussion';
import type { Thread } from '../../types/models';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { resetUseStickToBottomModuleStateForTest } from '../../utils/useStickToBottom.svelte';

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

// Controllable ResizeObserver. The global stub from `test/setup.ts` is a
// no-op; happy-dom's native RO doesn't fire on stubbed-getter geometry
// changes either. We install our own observer so wheel-up + escape +
// content-grow tests can fire the RO callback explicitly with a
// chosen height — that's the only way to exercise the spring-driver
// path without a real layout engine.
class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];
  callback: ResizeObserverCallback;
  observed: Element[] = [];
  constructor(cb: ResizeObserverCallback) {
    this.callback = cb;
    FireableResizeObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.push(el);
  }
  unobserve(): void {}
  disconnect(): void {
    this.observed = [];
  }
  fire(el: Element, height: number): void {
    this.callback(
      [
        {
          target: el,
          contentRect: {
            height, width: 0, top: 0, left: 0, right: 0, bottom: 0, x: 0, y: 0,
            toJSON: () => ({}),
          } as DOMRectReadOnly,
          borderBoxSize: [], contentBoxSize: [], devicePixelContentBoxSize: [],
        } as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }
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
  let originalRO: typeof ResizeObserver | undefined;

  beforeEach(async () => {
    vi.useFakeTimers();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    // Reset the useStickToBottom module-global mouseDown flag. Without
    // this, a prior test (or a prior file in the same Vitest worker)
    // that fired mousedown without a matching mouseup/click would leak
    // mouseDown=true into this file, where the controller's
    // isSelectingInside() would silently pause the spring.
    resetUseStickToBottomModuleStateForTest();
    // Install the controllable RO. Tests that don't need to fire it
    // (the existing six cases) just don't call .fire() — observe is
    // a no-op so they're undisturbed.
    FireableResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof FireableResizeObserver }).ResizeObserver
      = FireableResizeObserver;
  });

  afterEach(() => {
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver
        = originalRO;
    }
    FireableResizeObserver.instances = [];
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

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel Message Input"]')!;
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

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel Message Input"]')!;
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

  it('initial channel load uses forceStickAndSettle so async row growth re-snaps instantly instead of spring-chasing', async () => {
    // Regression: previously the initial-load path called
    // `forceStick({animation:'instant'})`, which writes scrollTop once
    // against the current scrollHeight. svelte-streamdown's async
    // typesetting (shiki / KaTeX / mermaid / parseIncompleteMarkdown
    // rebalance) keeps growing message rows for hundreds of ms after
    // first paint. The single-shot write was stale by the time those
    // grows landed; the spring then chased the moving bottom visibly.
    // The fix is forceStickAndSettle(): snap once, then re-snap on every
    // positive content-RO delta during the settle window (default 350ms,
    // matching RETAIN_ANIMATION_DURATION_MS). After the window expires,
    // normal spring behavior resumes.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 1, content: 'first' }),
      makeMsg({ id: 'b', sequence: 2, content: 'second' }),
    ]);
    const { getByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    let scrollHeightValue = 1000;
    Object.defineProperty(scroll, 'scrollHeight', {
      configurable: true,
      get: () => scrollHeightValue,
    });
    Object.defineProperty(scroll, 'clientHeight', {
      configurable: true,
      get: () => 600,
    });

    // Initial poll completes; ChannelView calls forceStickAndSettle.
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    const ro = FireableResizeObserver.instances.at(-1);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;

    // First RO fire seeds previousHeight. forceStickAndSettle has
    // already written scrollTop to the current target (= 1000 - 1 - 600
    // = 399), so the first-fire branch is a no-op for scrollTop.
    ro.fire(contentEl, 400);
    expect(scroll.scrollTop).toBe(399);

    // Streamdown async typesetting #1: scrollHeight grows from 1000 to
    // 1100. With the spring path, scrollTop would converge to the new
    // target 499 over multiple rAF frames. With the settle path, it
    // re-snaps to 499 in the same frame as the RO callback.
    scrollHeightValue = 1100;
    ro.fire(contentEl, 500);
    expect(scroll.scrollTop).toBe(499);

    // Streamdown async typesetting #2: another grow. Re-snaps again.
    scrollHeightValue = 1200;
    ro.fire(contentEl, 600);
    expect(scroll.scrollTop).toBe(599);

    // No spring frames advance scrollTop further — the settle path
    // produces single-frame re-snaps, not a multi-frame chase. If a
    // future regression replaced settle with spring, scrollTop would
    // either lag the target on the immediate-after-fire assertion above
    // or continue advancing here.
    const after = scroll.scrollTop;
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(after);
  });

  it('reveals the scroll-to-bottom chip after a wheel-up gesture, ignores content arrivals while escaped, and hides it on forceStick', async () => {
    // The unified controller (useStickToBottom) treats wheel-up as the
    // canonical "I want to read above" intent signal — escapedFromLock
    // flips synchronously, geometric near-bottom flips false, the chip
    // becomes visible, and the spring-driver's RO callback path bails
    // out for as long as escapedFromLock stays true. Clicking the chip
    // calls forceStick which slams scrollTop back to bottom and clears
    // the escape, so the chip hides.
    const pane = await buildPane();
    let callCount = 0;
    setBindingMock('GetChannelMessages', async () => {
      callCount++;
      if (callCount === 1) return [makeMsg({ id: 'a', sequence: 1, content: 'first' })];
      return [makeMsg({ id: 'b', sequence: 2, content: 'second' })];
    });

    const { getByTestId, queryByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    let scrollHeightValue = 1000;
    Object.defineProperty(scroll, 'scrollHeight', {
      configurable: true,
      get: () => scrollHeightValue,
    });
    Object.defineProperty(scroll, 'clientHeight', {
      configurable: true,
      get: () => 600,
    });

    // Initial state: sticky + at-bottom, chip hidden. Fire the
    // first-mount RO callback so the controller seeds previousHeight.
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    const ro = FireableResizeObserver.instances.at(-1);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;
    ro.fire(contentEl, 400);
    expect(queryByTestId('scroll-to-bottom')).toBeNull();

    // User scrolls up partway, then lifts a real wheel-up gesture. The
    // wheel-up is the intent signal that flips escapedFromLock; without
    // it, the controller would treat scrollTop changes as RO-correlated
    // and would not change intent.
    scroll.scrollTop = 200;
    await fireEvent.scroll(scroll);
    await fireEvent.wheel(scroll, { deltaY: -100 });
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // Distance from bottom is 1000-200-600 = 200 (>70 threshold), and the
    // wheel-up has flipped escapedFromLock + cleared isAtBottomState,
    // so both inputs to `stick.isAtBottom` are false. Chip is visible.
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // Subsequent poll cycle: a new message arrives, contentEl grows,
    // and the RO fires a positive delta. The escape state must block
    // the spring — scrollTop stays at 200 and the chip stays visible.
    scrollHeightValue = 1100;
    await vi.advanceTimersByTimeAsync(2500);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    ro.fire(contentEl, 500);
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(200);
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // Click the chip → forceStick: clears escape, writes scrollTop to
    // target (= scrollHeight - 1 - clientHeight = 499), hides chip.
    await fireEvent.click(getByTestId('scroll-to-bottom'));
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(499);
    expect(queryByTestId('scroll-to-bottom')).toBeNull();
  });

  it('re-arms stickiness when the user posts a message while escaped', async () => {
    // Posting is an explicit "I want to follow this conversation"
    // signal — handlePost calls stick.forceStick() before awaiting the
    // PostChannelMessage binding. This regression-guards that wiring.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 1, content: 'first' }),
    ]);
    setBindingMock('PostChannelMessage', async () => {});

    const { container, getByRole, getByTestId, queryByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    Object.defineProperty(scroll, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scroll, 'clientHeight', { configurable: true, get: () => 600 });

    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    const ro = FireableResizeObserver.instances.at(-1);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;
    ro.fire(contentEl, 400);

    // Escape via wheel-up; verify chip becomes visible.
    scroll.scrollTop = 100;
    await fireEvent.scroll(scroll);
    await fireEvent.wheel(scroll, { deltaY: -100 });
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // User types and clicks Post. handlePost calls stick.forceStick()
    // synchronously, which slams scrollTop to target = 1000 - 1 - 600
    // = 399 and clears the escape. The chip must hide.
    const textarea = container.querySelector<HTMLTextAreaElement>(
      'textarea[aria-label="Channel Message Input"]',
    )!;
    await fireEvent.input(textarea, { target: { value: 'follow up' } });
    await fireEvent.click(getByRole('button', { name: /post/i }));
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    expect(scroll.scrollTop).toBe(399);
    expect(queryByTestId('scroll-to-bottom')).toBeNull();
  });

  it('re-pins scrollTop when the composer section resizes (concluded toggle)', async () => {
    // The composer section sits in a sibling flex region below the
    // scroll container. When it grows or shrinks (e.g., the
    // concluded toggle swaps the textarea+button for a "Discussion has
    // concluded" paragraph), the scroll container's clientHeight
    // changes — but useStickToBottom's content RO doesn't fire because
    // contentEl didn't change. ChannelView installs a second RO on the
    // composer section that calls notifyContentMaybeGrew(), and the
    // controller writes scrollTop = max(0, scrollHeight - 1 -
    // clientHeight) so the user stays pinned to the last message.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 1, content: 'first' }),
    ]);

    const { getByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    let scrollHeightValue = 1000;
    let clientHeightValue = 600;
    Object.defineProperty(scroll, 'scrollHeight', {
      configurable: true,
      get: () => scrollHeightValue,
    });
    Object.defineProperty(scroll, 'clientHeight', {
      configurable: true,
      get: () => clientHeightValue,
    });

    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 5; i++) await Promise.resolve();

    // Two ROs are installed: the controller's contentEl RO and
    // ChannelView's composer-section RO. Identify the composer RO by
    // its testid'd target so a future style refactor (border-t →
    // something else) doesn't silently misroute the lookup.
    const composerSection = getByTestId('channel-composer-section');
    const composerRO = FireableResizeObserver.instances.find(
      (r) => r.observed[0] === composerSection,
    );
    if (!composerRO) throw new Error('expected ChannelView to install a composer RO');
    const composerEl = composerRO.observed[0] as HTMLElement;

    // Seed: user is sticky + at-bottom. Fire the content RO once so
    // previousHeight is set inside the controller.
    const contentRO = FireableResizeObserver.instances.find((r) => r !== composerRO);
    if (!contentRO) throw new Error('expected useStickToBottom to install a content RO');
    contentRO.fire(contentRO.observed[0], 400);
    scroll.scrollTop = 399; // = scrollHeight - 1 - clientHeight
    await fireEvent.scroll(scroll);
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // Now simulate the concluded toggle shrinking the composer (the
    // <p> "Discussion has concluded" line is shorter than the
    // textarea+button). clientHeight grows because flex re-distributes
    // the freed space to the scroll container. Without notify, the
    // controller wouldn't know; with notify, scrollTop is re-pinned to
    // the new target.
    clientHeightValue = 640;
    composerRO.fire(composerEl, 28);

    // notifyContentMaybeGrew runs synchronously inside the RO callback,
    // so the re-pin is observable on the next microtask.
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // New target = 1000 - 1 - 640 = 359. Controller wrote scrollTop to
    // this value via the notify path.
    expect(scroll.scrollTop).toBe(359);
  });
});
