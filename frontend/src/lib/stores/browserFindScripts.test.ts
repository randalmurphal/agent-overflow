// Replays the browser find scripts the Go tables record
// (internal/browser/find_pairs_test.go) against the real provider,
// registered with the real pane-find store as the browser pane registers it.
// Each script is one provider's view of a sequence the Manager ran, in
// order: what the reader did, the page the pane shows, each call the
// provider sent, the answer to its oldest unanswered call (which can come
// several reader actions later), each end the thread's providers were told
// of, a typed query's debounce running out, and the provider's stop retry
// wait running out. The provider must send exactly those calls, sequences
// included, leave no timer behind, and end with its bar open and showing
// exactly what the port left. The Go side checks the Manager against the
// same sequences and models the provider by a port of it, so the two cannot
// drift apart.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyBrowserFindEvent, BROWSER_FIND_DEBOUNCE_MS, BROWSER_FIND_STOP_RETRY_MS, createBrowserFind, resetBrowserFindForTest,
} from './browserFind.svelte';
import { BrowserCompanionEvent, BrowserFindState } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  closePaneFind, isPaneFindOpen, openPaneFind, registerPaneFindProvider, resetPaneFindForTest, setPaneFindQuery, stepPaneFind,
} from './paneFind.svelte';
import { TransportError } from '../transport/wsClient';
import pairs from '../../test/fixtures/browserFind/pairs.json';
import triples from '../../test/fixtures/browserFind/triples.json';
import inside from '../../test/fixtures/browserFind/inside.json';
import outstanding from '../../test/fixtures/browserFind/outstanding.json';
import named from '../../test/fixtures/browserFind/named.json';

interface ScriptCall {
  kind: string;
  page: string;
  seq: number;
  query?: string;
}

interface ScriptStep {
  ui?: 'open' | 'search' | 'type' | 'next' | 'clear' | 'dispose';
  query?: string;
  debounce?: boolean;
  shown?: string;
  send?: ScriptCall;
  answer?: Record<string, unknown>;
  error?: boolean;
  event?: Record<string, unknown>;
  retry?: boolean;
}

interface Script {
  name: string;
  mount: string;
  page: string;
  steps: ScriptStep[];
  bar: boolean;
  view: { count: number; current: number; pending: boolean };
}

interface SentCall {
  kind: string;
  pageId: string;
  seq: number;
  query?: string;
}

const THREAD = 'thread';
const PANE = 'browser-main';

async function replay(script: Script): Promise<void> {
  const replies: Array<{ resolve: (s: BrowserFindState) => void; reject: (e: Error) => void }> = [];
  const sent: SentCall[] = [];
  setBindingMock('BrowserCompanionFind', (_thread: string, action: SentCall) => {
    sent.push(action);
    return new Promise<BrowserFindState>((resolve, reject) => { replies.push({ resolve, reject }); });
  });
  const translate = (state: Record<string, unknown>) => new BrowserFindState(state as Partial<BrowserFindState>);
  let page = script.page;
  let disposed = false;
  let checked = 0;
  const find = createBrowserFind({
    threadId: THREAD, paneId: PANE, mode: 'bar', pageId: () => page, mount: () => script.mount,
    closeBar: () => closePaneFind(PANE), onError: () => {},
  });
  const release = registerPaneFindProvider(PANE, find);
  try {
    for (const [index, step] of script.steps.entries()) {
      const at = `step ${index}`;
      if (step.ui) {
        switch (step.ui) {
          case 'open':
            openPaneFind(PANE);
            break;
          case 'search':
          case 'type':
            setPaneFindQuery(PANE, step.query ?? '');
            break;
          case 'next':
            stepPaneFind(PANE, 1);
            break;
          case 'clear':
            closePaneFind(PANE);
            break;
          case 'dispose':
            disposed = true;
            release();
            find.dispose();
            break;
        }
        await vi.advanceTimersByTimeAsync(step.ui === 'search' || step.ui === 'open' ? BROWSER_FIND_DEBOUNCE_MS : 0);
      } else if (step.debounce) {
        await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
      } else if (step.shown) {
        page = step.shown;
      } else if (step.retry) {
        await vi.advanceTimersByTimeAsync(BROWSER_FIND_STOP_RETRY_MS);
      } else if (step.send) {
        const want = step.send;
        expect(sent.length, `${at}: the provider sent ${want.kind}`).toBeGreaterThan(checked);
        const got = sent[checked++];
        expect({ kind: got.kind, page: got.pageId, seq: got.seq, query: got.query ?? '' }, `${at}: the call`)
          .toEqual({ kind: want.kind, page: want.page, seq: want.seq, query: want.query ?? '' });
      } else if (step.event) {
        const state = translate(step.event);
        applyBrowserFindEvent(new BrowserCompanionEvent({ kind: 'find', threadId: THREAD, pageId: state.pageId, find: state }));
        await vi.advanceTimersByTimeAsync(0);
      } else {
        const reply = replies.shift();
        expect(reply, `${at}: a call waits for its answer`).toBeDefined();
        if (step.error) reply?.reject(new TransportError('method_error', 'refused'));
        else reply?.resolve(translate(step.answer ?? {}));
        await vi.advanceTimersByTimeAsync(0);
      }
    }
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    expect(sent.length, 'the provider sent nothing the script did not').toBe(checked);
    expect(replies.length, 'every call the provider sent was answered').toBe(0);
    expect(vi.getTimerCount(), 'no timer of the provider outlives what it serves').toBe(0);
    expect(isPaneFindOpen(PANE), 'the bar is open exactly as the port left it').toBe(script.bar);
    expect({ count: find.result.count ?? -1, current: find.result.current, pending: find.result.pending }, 'the bar shows what the port left')
      .toEqual(script.view);
  } finally {
    if (!disposed) {
      release();
      find.dispose();
    }
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  resetBrowserFindForTest();
  resetPaneFindForTest();
});

afterEach(() => {
  vi.useRealTimers();
  resetBrowserFindForTest();
  resetPaneFindForTest();
});

describe.each([
  ['pairs', pairs as Script[]],
  ['triples', triples as Script[]],
  ['actions with an end inside', inside as Script[]],
  ['a call outstanding across later actions', outstanding as Script[]],
  ['named sequences', named as Script[]],
])('browser find scripts: %s', (_table, scripts) => {
  it('has scripts to replay', () => {
    expect(scripts.length).toBeGreaterThan(0);
  });
  it.each(scripts.map((script) => [script.name, script] as const))('%s', async (_name, script) => {
    await replay(script);
  });
});
