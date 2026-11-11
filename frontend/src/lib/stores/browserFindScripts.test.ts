// Replays the browser find scripts the Go tables record
// (internal/browser/find_pairs_test.go) against the real provider. Each
// script is one provider's view of a sequence the Manager ran, in order:
// what the reader did, the page the pane shows, each call the provider sent,
// the answer to its oldest unanswered call (which can come several reader
// actions later), and each end the thread's providers were told of. The
// provider must send exactly those calls, and end with its bar shown
// exactly when the Manager holds a bar for its open. The Go side checks the
// Manager against the same sequences and models the provider by a port of
// it, so the two cannot drift apart.
//
// Open ids are the provider's own: a script names them by label (A1, B2),
// and the replay binds each label to the id the provider sent with it first.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { applyBrowserFindEvent, BROWSER_FIND_DEBOUNCE_MS, createBrowserFind, resetBrowserFindForTest } from './browserFind.svelte';
import { BrowserCompanionEvent, BrowserFindState } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetPaneFindForTest } from './paneFind.svelte';
import { TransportError } from '../transport/wsClient';
import pairs from '../../test/fixtures/browserFind/pairs.json';
import triples from '../../test/fixtures/browserFind/triples.json';
import inside from '../../test/fixtures/browserFind/inside.json';
import outstanding from '../../test/fixtures/browserFind/outstanding.json';

interface ScriptCall {
  kind: string;
  page: string;
  open: string;
  query?: string;
}

interface ScriptStep {
  ui?: 'opened' | 'search' | 'next' | 'clear' | 'dispose';
  query?: string;
  shown?: string;
  send?: ScriptCall;
  answer?: Record<string, unknown>;
  error?: boolean;
  event?: Record<string, unknown>;
}

interface Script {
  name: string;
  mount: string;
  page: string;
  steps: ScriptStep[];
  bar: boolean;
}

interface SentCall {
  kind: string;
  pageId: string;
  open: string;
  query?: string;
}

const THREAD = 'thread';

async function replay(script: Script): Promise<void> {
  const replies: Array<{ resolve: (s: BrowserFindState) => void; reject: (e: Error) => void }> = [];
  const sent: SentCall[] = [];
  setBindingMock('BrowserCompanionFind', (_thread: string, action: SentCall) => {
    sent.push(action);
    return new Promise<BrowserFindState>((resolve, reject) => { replies.push({ resolve, reject }); });
  });
  const opens = new Map<string, string>();
  // An open the provider never sent (another provider's) stays as named.
  const translate = (state: Record<string, unknown>) => {
    const open = String(state.open ?? '');
    return new BrowserFindState({ ...(state as Partial<BrowserFindState>), open: opens.get(open) ?? open });
  };
  let page = script.page;
  let shown = false;
  let disposed = false;
  let checked = 0;
  const find = createBrowserFind({
    threadId: THREAD, paneId: 'browser-main', mode: 'bar', pageId: () => page, mount: () => script.mount,
    onEnded: () => { shown = false; }, onError: () => {},
  });
  try {
    for (const [index, step] of script.steps.entries()) {
      const at = `step ${index}`;
      if (step.ui) {
        switch (step.ui) {
          case 'opened':
            shown = true;
            find.opened?.();
            break;
          case 'search':
            find.search(step.query ?? '');
            break;
          case 'next':
            find.next?.();
            break;
          case 'clear':
            shown = false;
            find.clear();
            break;
          case 'dispose':
            shown = false;
            disposed = true;
            find.dispose();
            break;
        }
        await vi.advanceTimersByTimeAsync(step.ui === 'search' ? BROWSER_FIND_DEBOUNCE_MS : 0);
      } else if (step.shown) {
        page = step.shown;
      } else if (step.send) {
        const want = step.send;
        expect(sent.length, `${at}: the provider sent ${want.kind}`).toBeGreaterThan(checked);
        const got = sent[checked++];
        expect({ kind: got.kind, page: got.pageId, query: got.query ?? '' }, `${at}: the call`)
          .toEqual({ kind: want.kind, page: want.page, query: want.query ?? '' });
        const bound = opens.get(want.open);
        if (bound === undefined) {
          expect([...opens.values()], `${at}: open ${want.open} is a new open`).not.toContain(got.open);
          expect(got.open, `${at}: open ${want.open} has an id`).not.toBe('');
          opens.set(want.open, got.open);
        } else {
          expect(got.open, `${at}: the call names open ${want.open}`).toBe(bound);
        }
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
    expect(shown, 'the bar is shown exactly when the Manager holds one for this open').toBe(script.bar);
  } finally {
    if (!disposed) find.dispose();
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
])('browser find scripts: %s', (_table, scripts) => {
  it('has scripts to replay', () => {
    expect(scripts.length).toBeGreaterThan(0);
  });
  it.each(scripts.map((script) => [script.name, script] as const))('%s', async (_name, script) => {
    await replay(script);
  });
});
