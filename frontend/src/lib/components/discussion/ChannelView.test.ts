import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChannelView from './ChannelView.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { ChannelMessage, ChannelStatePayload } from '../../types/discussion';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { lookupDiscussionLiveTail, clearAllDiscussionLiveTail } from '../../stores/discussionLiveTail';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { makeChannelMessage, makeChannelStatePayload } from '../../../test/helpers/discussion';

// This suite covers ChannelView's push-driven rewrite: no polling, an
// authoritative discussion:state-derived header, live-tail streaming for
// the current speaker, and the same spring/sync-pin scroll choreography
// MessageTimeline uses. Event ROUTING (discussion:message / discussion:state
// / the provider:item_event live-tail seam) has its own coverage in
// events.discussion.test.ts — here we drive the pane's applyChannel*
// methods directly (what that routing layer calls) to test ChannelView's
// rendering in isolation.

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
// chosen height — that's the only way to exercise the controller's
// content-RO path without a real layout engine.
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
  return makeBaseThread({
    id: 'parent-thread',
    title: 'Deliberation',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'discussion',
    discussionId: 'channel-1',
  });
}

// This suite's historical defaults: short 'm<seq>' ids with sequence 0,
// a non-roster fromId, and an empty FSM snapshot (no current speaker,
// no roster) so rendering tests opt IN to live-tail state explicitly.
function makeMsg(overrides: Partial<ChannelMessage> = {}): ChannelMessage {
  return makeChannelMessage({
    id: 'm' + (overrides.sequence ?? 0),
    sequence: 0,
    fromId: 'agent-id',
    ...overrides,
  });
}

function makeState(overrides: Partial<ChannelStatePayload> = {}): ChannelStatePayload {
  return makeChannelStatePayload({
    currentSpeakerThreadId: '',
    currentSpeakerRole: '',
    participants: [],
    ...overrides,
  });
}

async function buildPane(thread = makeThread()) {
  return buildRegisteredPane(thread);
}

// Advance past the initial-load promise chain (Promise.all(GetChannelState,
// GetChannelMessages) → applyChannelState/applyChannelMessages → tick() →
// forceStick). Fake timers still need draining because loadInitial awaits
// a real microtask chain even with instant-resolving mocks.
async function settleInitialLoad(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

describe('<ChannelView>', () => {
  let originalRO: typeof ResizeObserver | undefined;

  beforeEach(async () => {
    vi.useFakeTimers();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    // Reset the intent machine module-global mouseDown flag. Without
    // this, a prior test (or a prior file in the same Vitest worker)
    // that fired mousedown without a matching mouseup/click would leak
    // mouseDown=true into this file, where the controller's
    // isSelectingInside() would silently suppress sync-pin writes.
    resetScrollIntentModuleStateForTest();
    // Default channel-state/message mocks so tests that don't care about
    // the FSM snapshot don't have to stub GetChannelState explicitly —
    // an unmocked binding throws, which loadInitial would otherwise
    // surface as a spurious error banner in unrelated tests.
    setBindingMock('GetChannelState', async () => makeState());
    setBindingMock('GetChannelMessages', async () => []);
    // Install the controllable RO. Tests that don't need to fire it
    // just don't call .fire() — observe is a no-op so they're undisturbed.
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
    resetPanesForTest();
    clearAllDiscussionLiveTail();
  });

  it('opts out of browser scroll-anchor on the scroll container', async () => {
    // Regression guard: the browser's default `overflow-anchor: auto`
    // adjusts scrollTop to keep the topmost-visible element fixed when
    // content above the viewport changes size — which fights the
    // controller's contentRO sync-pin. Streamdown async typesetting
    // (shiki / KaTeX / mermaid) growing rows above the viewport on a
    // sticky session would produce visible scrollTop oscillation between
    // the browser's anchor adjustment and our re-pin without this
    // opt-out.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 0, content: 'first' }),
    ]);
    const { getByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    expect(scroll.style.overflowAnchor).toBe('none');
  });

  it('loads with afterSeq -1 and renders a sequence-0 message (regression: afterSeq=0 used to hide it)', async () => {
    // Message sequences are zero-based. The old poll cursor started at
    // afterSeq=0, which is an EXCLUSIVE bound — so a channel's very
    // first message (sequence 0) never rendered on a fresh load. -1 is
    // the correct "fetch everything" cursor.
    const pane = await buildPane();
    const getMock = setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 0, content: 'first', fromRole: 'advocate' }),
      makeMsg({ id: 'b', sequence: 1, content: 'second', fromRole: 'critic' }),
    ]);
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    expect(await findByText('first')).toBeInTheDocument();
    expect(await findByText('second')).toBeInTheDocument();
    expect(getMock.mock.calls[0]).toEqual(['channel-1', -1, 500]);
    expect(pane.channelMessages.map((m) => m.sequence)).toEqual([0, 1]);
  });

  it('does not poll: GetChannelMessages is called once regardless of elapsed time', async () => {
    const pane = await buildPane();
    const getMock = setBindingMock('GetChannelMessages', async () => [makeMsg({ sequence: 0 })]);
    render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();
    expect(getMock).toHaveBeenCalledTimes(1);

    // Advance well past the old 2.5s poll interval and its backoff ceiling.
    await vi.advanceTimersByTimeAsync(10_000);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(getMock).toHaveBeenCalledTimes(1);
  });

  it('applies a discussion:message push without duplicating an already-loaded sequence', async () => {
    // Push-driven equivalent of the old poll-merge test: simulates what
    // eventsDiscussion.ts's applyDiscussionMessage does on a real
    // discussion:message event (dedupe-by-sequence itself is unit-tested
    // in threadChannelState.svelte.test.ts; this checks ChannelView
    // renders the merge).
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [makeMsg({ id: 'a', sequence: 0, content: 'first' })]);
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    expect(await findByText('first')).toBeInTheDocument();

    pane.applyChannelMessage(makeMsg({ id: 'a', sequence: 0, content: 'first' })); // duplicate, no-op
    pane.applyChannelMessage(makeMsg({ id: 'b', sequence: 1, content: 'second' }));
    await tick();

    expect(pane.channelMessages.map((m) => m.sequence)).toEqual([0, 1]);
    expect(await findByText('second')).toBeInTheDocument();
  });

  it('posts via PostChannelMessage and immediately applies the returned message', async () => {
    const postMock = setBindingMock('PostChannelMessage', async () =>
      makeMsg({ id: 'posted-1', sequence: 0, fromType: 'human', fromId: 'human', fromRole: undefined, content: 'my intervention' }));
    const pane = await buildPane();
    const { container, getByRole, findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel Message Input"]')!;
    await fireEvent.input(textarea, { target: { value: 'my intervention' } });
    await fireEvent.click(getByRole('button', { name: /post/i }));
    for (let i = 0; i < 5; i++) await Promise.resolve();

    expect(postMock.mock.calls[0]).toEqual(['channel-1', 'my intervention']);
    expect(textarea.value).toBe('');
    // Applied immediately from PostChannelMessage's own return value, not
    // from a subsequent discussion:message echo (that echo's sequence
    // dedupe would make it a no-op — see threadChannelState.svelte.test.ts).
    expect(pane.channelMessages).toHaveLength(1);
    expect(await findByText('my intervention')).toBeInTheDocument();
    expect(await findByText('You')).toBeInTheDocument();
  });

  it('rolls back the textarea when PostChannelMessage rejects', async () => {
    setBindingMock('PostChannelMessage', async () => { throw new Error('rpc-down'); });
    const pane = await buildPane();
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { container, getByRole } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Channel Message Input"]')!;
    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByRole('button', { name: /post/i }));
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(textarea.value).toBe('fails');
    expect(pane.channelMessages).toHaveLength(0);
    consoleErr.mockRestore();
  });

  it('shows an error banner on initial-load failure and recovers via Retry', async () => {
    let callCount = 0;
    setBindingMock('GetChannelMessages', async () => {
      callCount++;
      if (callCount === 1) throw new Error('rpc-down');
      return [makeMsg({ sequence: 0, content: 'recovered' })];
    });
    const pane = await buildPane();
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByRole, findByText, queryByText } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    await settleInitialLoad();

    expect(await findByText(/failed to load channel/i)).toBeInTheDocument();

    await fireEvent.click(getByRole('button', { name: /retry/i }));
    await settleInitialLoad();

    expect(await findByText('recovered')).toBeInTheDocument();
    expect(queryByText(/failed to load channel/i)).toBeNull();
    consoleErr.mockRestore();
  });

  it('retry does not race itself: a slower first retry cannot clobber a newer one', async () => {
    // retryInitialLoad must claim a fresh generation. Reusing the
    // current one let two rapid Retry clicks run two un-cancellable
    // concurrent loads with the SLOWER resolver winning — a stale
    // failure landing after the newer retry's success re-raised the
    // error banner over recovered content.
    let callCount = 0;
    let rejectFirstRetry: ((err: Error) => void) | undefined;
    setBindingMock('GetChannelMessages', async () => {
      callCount++;
      if (callCount === 1) throw new Error('initial-down'); // initial load → Retry banner
      if (callCount === 2) {
        // First Retry click: hangs until we reject it below.
        return await new Promise<ChannelMessage[]>((_, reject) => {
          rejectFirstRetry = reject;
        });
      }
      return [makeMsg({ sequence: 0, content: 'second retry wins' })];
    });
    const pane = await buildPane();
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByRole, findByText, queryByText } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    await settleInitialLoad();
    expect(await findByText(/failed to load channel/i)).toBeInTheDocument();

    // Two rapid clicks on the same Retry button, dispatched before any
    // re-render can swap the banner for the loading placeholder.
    const retryButton = getByRole('button', { name: /retry/i });
    const firstClick = fireEvent.click(retryButton);
    const secondClick = fireEvent.click(retryButton);
    await firstClick;
    await secondClick;
    await settleInitialLoad();

    // The second (newest) retry resolved: content is up.
    expect(await findByText('second retry wins')).toBeInTheDocument();

    // Now the FIRST retry fails late. Its generation is stale, so it
    // must not re-raise the error banner over the newer success.
    expect(rejectFirstRetry).toBeDefined();
    rejectFirstRetry!(new Error('slow-stale-failure'));
    await settleInitialLoad();

    expect(queryByText(/failed to load channel/i)).toBeNull();
    expect(await findByText('second retry wins')).toBeInTheDocument();
    consoleErr.mockRestore();
  });

  it('disables posting, hides the composer, and suppresses the speaking indicator once concluded', async () => {
    setBindingMock('GetChannelState', async () =>
      makeState({ status: 'concluded', awaitingResponse: true, currentSpeakerRole: 'critic' }));
    const pane = await buildPane();
    const { getAllByText, queryByRole, queryByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    await settleInitialLoad();

    const concludedMatches = getAllByText(/concluded/i);
    expect(concludedMatches.length).toBeGreaterThanOrEqual(1);
    // Composer is hidden when concluded, so there is no Post button.
    expect(queryByRole('button', { name: /post/i })).toBeNull();
    // status !== 'open' gates the speaking indicator even though
    // awaitingResponse is (implausibly) still true in this payload.
    expect(queryByTestId('channel-speaking-indicator')).toBeNull();
  });

  it('shows the turn counter and current-speaker label from the channel-state snapshot', async () => {
    setBindingMock('GetChannelState', async () =>
      makeState({ turnCount: 3, maxTurns: 8, awaitingResponse: true, currentSpeakerRole: 'critic' }));
    const pane = await buildPane();
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();

    expect(await findByText('Turn 3 of 8')).toBeInTheDocument();
    expect(await findByText(/Speaking: critic/)).toBeInTheDocument();
  });

  it('shows a speaking indicator while awaitingResponse and no live-tail text has arrived yet', async () => {
    const pane = await buildPane();
    const { queryByTestId, getByTestId } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();

    expect(queryByTestId('channel-speaking-indicator')).toBeNull();

    pane.applyChannelState(makeState({ awaitingResponse: true, currentSpeakerRole: 'critic' }));
    await tick();

    const indicator = getByTestId('channel-speaking-indicator');
    expect(indicator.textContent).toMatch(/critic is preparing a response/i);
  });

  it('streams the current speaker live tail as a card, then swaps it for the settled agent message', async () => {
    const pane = await buildPane();
    const { findByText, queryByText, queryByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    await settleInitialLoad();

    // discussion:state seeds the roster — registers 'advocate-thread' in
    // the discussionLiveTail registry and marks it as the current speaker.
    pane.applyChannelState(makeState({
      awaitingResponse: true,
      currentSpeakerThreadId: 'advocate-thread',
      currentSpeakerRole: 'advocate',
      participants: [{ threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6' }],
    }));

    // eventsItemStream.ts's live-tail seam feeds assistant_text upserts
    // from the (unmounted) advocate child thread through the registry.
    const handlers = lookupDiscussionLiveTail('advocate-thread');
    expect(handlers?.size).toBe(1);
    for (const handler of handlers!) handler.applyTailUpsert('advocate-thread', 'item-1', 'partial reply');
    await tick();

    expect(await findByText('partial reply')).toBeInTheDocument();
    expect(queryByTestId('channel-speaking-indicator')).toBeNull();

    // The advocate's turn settles: discussion:message lands with the
    // final text (clearing the tail — same fromId as the message),
    // followed by a discussion:state push resolving awaitingResponse.
    pane.applyChannelMessage(makeMsg({
      id: 'final-1', sequence: 0, fromType: 'agent', fromId: 'advocate-thread', fromRole: 'advocate', content: 'final reply',
    }));
    pane.applyChannelState(makeState({ awaitingResponse: false }));
    await tick();

    expect(await findByText('final reply')).toBeInTheDocument();
    expect(queryByText('partial reply')).toBeNull();
    expect(pane.channelLiveTail).toBeNull();
    expect(queryByTestId('channel-speaking-indicator')).toBeNull();
  });

  it('badges system messages as Moderator', async () => {
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 0, fromType: 'system', fromId: 'system', fromRole: undefined, content: 'Discussion started.' }),
    ]);
    const pane = await buildPane();
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    expect(await findByText('Discussion started.')).toBeInTheDocument();
    expect(await findByText('Moderator')).toBeInTheDocument();
  });

  it('shows empty-state copy when the channel has no messages, tail, or speaking indicator', async () => {
    const pane = await buildPane();
    const { findByText } = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
    await settleInitialLoad();
    expect(await findByText('No messages yet. Post a message to start the discussion.')).toBeInTheDocument();
  });

  it('initial channel load + async row growth sync-pins scrollTop in the same paint as each contentRO delta', async () => {
    // Regression: before sync-pin landed, autonomous content growth on
    // a sticky session armed a spring driver that chased the moving
    // bottom visibly. svelte-streamdown's async typesetting (shiki /
    // KaTeX / mermaid / parseIncompleteMarkdown rebalance) keeps
    // growing message rows for hundreds of ms after first paint, so
    // the chase showed up as a top-to-bottom scroll preamble on every
    // channel load. The fix routes autonomous growth through the
    // contentRO's synchronous re-pin: each positive delta writes
    // scrollTop to the new target inside the RO callback, so the
    // browser only paints the final state per frame.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 0, content: 'first' }),
      makeMsg({ id: 'b', sequence: 1, content: 'second' }),
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

    // Initial load completes; ChannelView's post-load forceStick()
    // writes scrollTop to the current target (= 1000 - 600 = 400).
    await settleInitialLoad();
    const composerSection = getByTestId('channel-composer-section');
    const ro = FireableResizeObserver.instances.find((observer) => observer.observed[0] !== composerSection);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;

    // First RO fire seeds previousHeight. The first-fire branch writes
    // scrollTop to the current target if sticky; we're already at 400,
    // so this is a no-op visually.
    ro.fire(contentEl, 400);
    expect(scroll.scrollTop).toBe(400);

    // Streamdown async typesetting #1: scrollHeight grows to 1100.
    // contentRO's positive-delta sync-pin writes scrollTop=500 in the
    // same frame as the callback. No rAF gap.
    scrollHeightValue = 1100;
    ro.fire(contentEl, 500);
    expect(scroll.scrollTop).toBe(500);

    // Streamdown async typesetting #2: another grow. Same single-write
    // convergence.
    scrollHeightValue = 1200;
    ro.fire(contentEl, 600);
    expect(scroll.scrollTop).toBe(600);

    // No rAF frames advance scrollTop further. If a future regression
    // re-introduced an autonomous chase loop on contentRO deltas,
    // scrollTop would lag target on the immediate-after-fire
    // assertions above or continue advancing here.
    const after = scroll.scrollTop;
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(after);
  });

  it('reveals the scroll-to-bottom chip after a wheel-up gesture, ignores content arrivals while escaped, and hides it on forceStick', async () => {
    // The unified controller (useStickToBottom) treats wheel-up as an
    // escape intent and confirms it only when the outer scroller moves
    // upward. Then geometric near-bottom flips false, the chip becomes
    // visible, and the contentRO's sync-pin path bails while escaped.
    // Clicking the chip calls forceStick, which slams scrollTop back to
    // bottom and clears the escape.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [makeMsg({ id: 'a', sequence: 0, content: 'first' })]);

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
    await settleInitialLoad();
    const composerSection = getByTestId('channel-composer-section');
    const ro = FireableResizeObserver.instances.find((observer) => observer.observed[0] !== composerSection);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;
    ro.fire(contentEl, 400);
    expect(queryByTestId('scroll-to-bottom')).toBeNull();

    // User lifts a real wheel-up gesture and the outer scroller moves up.
    scroll.scrollTop = 400;
    await fireEvent.wheel(scroll, { deltaY: -100 });
    scroll.scrollTop = 200;
    await fireEvent.scroll(scroll);
    await vi.advanceTimersByTimeAsync(16);
    await tick();
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // Distance from bottom is 1000-200-600 = 200 (>70 threshold), and the
    // wheel-up scroll has flipped escapedFromLock + cleared isAtBottomState,
    // so both inputs to `stick.isAtBottom` are false. Chip is visible.
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // A discussion:message push lands (simulated directly on the pane —
    // the push-driven equivalent of the old poll cycle) and contentEl
    // grows; the RO fires a positive delta. The escape state must block
    // the sync-pin — scrollTop stays at 200 and the chip stays visible.
    pane.applyChannelMessage(makeMsg({ id: 'b', sequence: 1, content: 'second' }));
    await tick();
    scrollHeightValue = 1100;
    ro.fire(contentEl, 500);
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(200);
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // Click the chip → forceStick: clears escape, writes scrollTop to
    // target (= scrollHeight - clientHeight = 500), hides chip.
    await fireEvent.click(getByTestId('scroll-to-bottom'));
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(scroll.scrollTop).toBe(500);
    expect(queryByTestId('scroll-to-bottom')).toBeNull();
  });

  it('keeps the user escaped when posting while reading above the bottom', async () => {
    // Posting preserves current scroll intent. If the user has escaped
    // upward to read earlier discussion, sending a post must not yank
    // them back to the bottom.
    setBindingMock('GetChannelMessages', async () => [makeMsg({ id: 'a', sequence: 0, content: 'first' })]);
    setBindingMock('PostChannelMessage', async () => makeMsg({ id: 'b', sequence: 1, fromType: 'human', fromId: 'human', fromRole: undefined, content: 'follow up' }));
    const pane = await buildPane();

    const { container, getByRole, getByTestId } = render(ChannelView, {
      props: { pane, channelId: 'channel-1' },
    });
    const scroll = getByTestId('channel-message-list') as HTMLElement;
    let scrollHeightValue = 1000;
    Object.defineProperty(scroll, 'scrollHeight', {
      configurable: true,
      get: () => scrollHeightValue,
    });
    Object.defineProperty(scroll, 'clientHeight', { configurable: true, get: () => 600 });

    await settleInitialLoad();
    const composerSection = getByTestId('channel-composer-section');
    const ro = FireableResizeObserver.instances.find((observer) => observer.observed[0] !== composerSection);
    if (!ro) throw new Error('expected useStickToBottom to install a ResizeObserver');
    const contentEl = ro.observed[0] as HTMLElement;
    ro.fire(contentEl, 400);

    // Escape via wheel-up that moves the outer scroller; verify chip becomes visible.
    scroll.scrollTop = 400;
    await fireEvent.wheel(scroll, { deltaY: -100 });
    scroll.scrollTop = 100;
    await fireEvent.scroll(scroll);
    await vi.advanceTimersByTimeAsync(16);
    await tick();
    for (let i = 0; i < 3; i++) await Promise.resolve();
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();

    // User types and clicks Post. The existing escape must hold: no
    // forceStick, no target write, and the chip stays visible.
    const textarea = container.querySelector<HTMLTextAreaElement>(
      'textarea[aria-label="Channel Message Input"]',
    )!;
    await fireEvent.input(textarea, { target: { value: 'follow up' } });
    await fireEvent.click(getByRole('button', { name: /post/i }));
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 8; i++) await Promise.resolve();

    expect(scroll.scrollTop).toBe(100);
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();
    expect(pane.channelMessages.map((msg) => msg.id)).toContain('b');

    // The post's own applied message grows content. If that makes
    // content taller, the content RO still must respect the existing
    // user escape.
    scrollHeightValue = 1100;
    ro.fire(contentEl, 500);
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    expect(scroll.scrollTop).toBe(100);
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();
  });

  it('re-pins scrollTop when the composer section resizes (concluded toggle)', async () => {
    // The composer section sits in a sibling flex region below the
    // scroll container. When it grows or shrinks (e.g., the
    // concluded toggle swaps the textarea+button for a "Discussion has
    // concluded" paragraph), the scroll container's clientHeight
    // changes — but useStickToBottom's content RO doesn't fire because
    // contentEl didn't change. ChannelView installs a second RO on the
    // composer section that calls observe('composer-geometry'), and the
    // controller writes scrollTop = max(0, scrollHeight - clientHeight)
    // so the user stays pinned to the last message.
    const pane = await buildPane();
    setBindingMock('GetChannelMessages', async () => [
      makeMsg({ id: 'a', sequence: 0, content: 'first' }),
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

    await settleInitialLoad();

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
    scroll.scrollTop = 400; // = scrollHeight - clientHeight
    await fireEvent.scroll(scroll);
    await vi.advanceTimersByTimeAsync(16);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // Now simulate the concluded toggle shrinking the composer (the
    // <p> "Discussion has concluded" line is shorter than the
    // textarea+button). clientHeight grows because flex re-distributes
    // the freed space to the scroll container. Without notify, the
    // controller wouldn't know; with notify, scrollTop is re-pinned to
    // the new target.
    pane.applyChannelState(makeState({ status: 'concluded' }));
    await tick();
    clientHeightValue = 640;
    composerRO.fire(composerEl, 28);

    // The observation pins synchronously inside the RO callback, so the
    // re-pin is observable on the next microtask.
    for (let i = 0; i < 3; i++) await Promise.resolve();

    // New target = 1000 - 640 = 360. Controller wrote scrollTop to
    // this value via the notify path.
    expect(scroll.scrollTop).toBe(360);
  });
});
