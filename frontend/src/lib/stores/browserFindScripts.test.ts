// Replays the browser find scripts the Go tables record
// (internal/browser/find_pairs_test.go) against the real provider. Each
// script is one mount's view of a sequence the Manager ran: the actions its
// provider sent, the ends that landed while each was in flight, its
// answers, and the ends that landed while it was idle. The provider must
// send exactly those actions, and end with its bar shown exactly when the
// Manager holds a bar for its mount. The Go side checks the Manager against
// the same sequences, so the provider's rules and the table's model of them
// cannot drift apart.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { applyBrowserFindEvent, BROWSER_FIND_DEBOUNCE_MS, createBrowserFind, resetBrowserFindForTest } from './browserFind.svelte';
import { BrowserCompanionEvent, BrowserFindState } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetPaneFindForTest } from './paneFind.svelte';
import pairs from '../../test/fixtures/browserFind/pairs.json';
import triples from '../../test/fixtures/browserFind/triples.json';
import inside from '../../test/fixtures/browserFind/inside.json';

interface ScriptStep {
  act?: 'open' | 'start' | 'next' | 'stop' | 'close';
  query?: string;
  answer?: Record<string, unknown>;
  error?: boolean;
  events?: Array<Record<string, unknown>>;
}

interface Script {
  name: string;
  mount: string;
  page: string;
  steps: ScriptStep[];
  bar: boolean;
}

const THREAD = 'thread';

function deliver(events: ScriptStep['events']): void {
  for (const event of events ?? []) {
    const find = new BrowserFindState(event as Partial<BrowserFindState>);
    applyBrowserFindEvent(new BrowserCompanionEvent({ kind: 'find', threadId: THREAD, pageId: find.pageId, find }));
  }
}

async function replay(script: Script): Promise<void> {
  const replies: Array<{ resolve: (s: BrowserFindState) => void; reject: (e: Error) => void }> = [];
  const sent: string[] = [];
  setBindingMock('BrowserCompanionFind', (_thread: string, action: { kind: string }) => {
    sent.push(action.kind);
    return new Promise<BrowserFindState>((resolve, reject) => { replies.push({ resolve, reject }); });
  });
  let shown = false;
  const find = createBrowserFind({
    threadId: THREAD, paneId: 'browser-main', mode: 'bar', pageId: () => script.page, mount: () => script.mount,
    onEnded: () => { shown = false; }, onError: () => {},
  });
  try {
    for (const [index, step] of script.steps.entries()) {
      if (!step.act) {
        deliver(step.events);
        await vi.advanceTimersByTimeAsync(0);
        continue;
      }
      const before = sent.length;
      switch (step.act) {
        case 'open':
          shown = true;
          find.opened?.();
          break;
        case 'start':
          find.search(step.query ?? '');
          break;
        case 'next':
          find.next?.();
          break;
        case 'stop':
          find.search('');
          break;
        case 'close':
          shown = false;
          find.clear();
          break;
      }
      await vi.advanceTimersByTimeAsync(step.act === 'start' ? BROWSER_FIND_DEBOUNCE_MS : 0);
      expect(sent.slice(before), `step ${index}: the provider's calls for ${step.act}`).toEqual([step.act]);
      deliver(step.events);
      const reply = replies.shift();
      expect(reply, `step ${index}: a call waits for its answer`).toBeDefined();
      if (step.error) reply?.reject(new Error('refused'));
      else reply?.resolve(new BrowserFindState(step.answer as Partial<BrowserFindState>));
      await vi.advanceTimersByTimeAsync(0);
    }
    await vi.advanceTimersByTimeAsync(BROWSER_FIND_DEBOUNCE_MS);
    const acted = script.steps.filter((step) => step.act).length;
    expect(sent.length, 'the provider sent nothing the script did not').toBe(acted);
    expect(shown, 'the bar is shown exactly when the Manager holds one for this mount').toBe(script.bar);
  } finally {
    find.dispose();
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
])('browser find scripts: %s', (_table, scripts) => {
  it('has scripts to replay', () => {
    expect(scripts.length).toBeGreaterThan(0);
  });
  it.each(scripts.map((script) => [script.name, script] as const))('%s', async (_name, script) => {
    await replay(script);
  });
});
