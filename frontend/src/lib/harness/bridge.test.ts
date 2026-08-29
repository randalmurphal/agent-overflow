// Dispatch rules: the union's version gate, the error envelope, and the
// globals whitelist. What is NOT here is anything timing-shaped (the rAF
// meter, the mutation clock's own cadence) — those need a real engine and
// are covered by e2e/tests/harness-bridge.spec.ts.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  MUTATION_CLOCK_LINGER_MS,
  answerHarnessQuery,
  isHarnessNoReply,
  mutationClockArmed,
  sinceLastMutationMs,
  stopHarnessBridge,
} from './bridge';
import { harnessGlobalNames } from './globals';
import { PERF_WATCHDOG_MS } from './perf';

interface ErrorEnvelope {
  error?: string;
}

afterEach(() => {
  stopHarnessBridge();
  delete (window as { __stickState?: unknown }).__stickState;
  delete (window as { __agentOverflowUiTrace?: unknown }).__agentOverflowUiTrace;
  delete (window as { __aoRevealDrain?: unknown }).__aoRevealDrain;
});

describe('answerHarnessQuery', () => {
  it('never rejects: a malformed spec comes back as an error envelope', async () => {
    for (const spec of [null, 42, 'viewport', []]) {
      const result = (await answerHarnessQuery(spec)) as ErrorEnvelope;
      expect(result.error).toContain('JSON object');
    }
  });

  it('refuses a version it does not speak', async () => {
    const result = (await answerHarnessQuery({ v: 2, kind: 'viewport' })) as ErrorEnvelope;
    expect(result.error).toContain('unsupported query version 2');
  });

  it('names an unknown kind rather than answering emptily', async () => {
    const result = (await answerHarnessQuery({ v: 1, kind: 'nope' })) as ErrorEnvelope;
    expect(result.error).toContain('unknown query kind "nope"');
    const missing = (await answerHarnessQuery({ v: 1 })) as ErrorEnvelope;
    expect(missing.error).toContain('no kind');
  });

  it('answers a viewport query with a versioned snapshot', async () => {
    document.body.innerHTML = '<section data-pane-id="p" data-pane-kind="chat"></section>';
    const result = (await answerHarnessQuery({ v: 1, kind: 'viewport' })) as {
      v: number;
      panes: unknown[];
    };
    expect(result.v).toBe(1);
    expect(result.panes).toHaveLength(1);
  });

  it('distinguishes a bad selector from a selector that matches nothing', async () => {
    const bad = (await answerHarnessQuery({ v: 1, kind: 'element', selector: '[[' })) as ErrorEnvelope;
    expect(bad.error).toContain('invalid selector');
    const empty = (await answerHarnessQuery({ v: 1, kind: 'element', selector: '.nope' })) as {
      count: number;
    };
    expect(empty.count).toBe(0);
    const missing = (await answerHarnessQuery({ v: 1, kind: 'element' })) as ErrorEnvelope;
    expect(missing.error).toContain('requires a selector');
  });

  it('only includes element scroll geometry when the query requests it', async () => {
    document.body.innerHTML = '<main id="target">content</main>';
    const plain = (await answerHarnessQuery({
      v: 1,
      kind: 'element',
      selector: '#target',
    })) as { first: { scroll?: unknown } };
    expect(plain.first.scroll).toBeUndefined();

    const withScroll = (await answerHarnessQuery({
      v: 1,
      kind: 'element',
      selector: '#target',
      includeScroll: true,
    })) as { first: { scroll?: unknown } };
    expect(withScroll.first.scroll).toBeDefined();
  });
});

describe('globals query', () => {
  it('answers an absent-but-whitelisted global with unavailable, not an error', async () => {
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'globals',
      name: '__paneGeometry',
    })) as { name: string; unavailable?: true; error?: string };
    expect(result.error).toBeUndefined();
    expect(result).toMatchObject({ v: 1, name: '__paneGeometry', unavailable: true });
  });

  it('reads a present global', async () => {
    (window as { __stickState?: () => unknown }).__stickState = () => ({ atBottom: true });
    const result = (await answerHarnessQuery({ v: 1, kind: 'globals', name: '__stickState' })) as {
      value: unknown;
    };
    expect(result.value).toEqual({ atBottom: true });
  });

  it('passes the count through to the ui-trace reader', async () => {
    (window as { __agentOverflowUiTrace?: unknown }).__agentOverflowUiTrace = {
      recent: (count = 50) => ({ count }),
    };
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'globals',
      name: 'uiTrace.recent',
      args: [7],
    })) as { value: { count: number } };
    expect(result.value).toEqual({ count: 7 });
  });

  // The whitelist IS the security surface of this query kind. A name
  // outside it must fail loudly, or a typo and an escape attempt look
  // identical from the outside.
  it('errors on a name outside the whitelist', async () => {
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'globals',
      name: 'localStorage',
    })) as ErrorEnvelope;
    expect(result.error).toContain('unknown global "localStorage"');
    expect(result.error).toContain('__stickState');
  });

  // The reader table is an object literal, so a plain `READERS[name]`
  // lookup resolves Object.prototype's own keys — `constructor` would have
  // answered with the Object constructor and been CALLED. Inherited keys
  // are not entries, and the whitelist has to say so.
  it('refuses prototype keys rather than resolving them through Object.prototype', async () => {
    for (const name of ['constructor', 'toString', '__proto__', 'hasOwnProperty', 'valueOf']) {
      const result = (await answerHarnessQuery({ v: 1, kind: 'globals', name })) as ErrorEnvelope;
      expect(result.error, name).toContain(`unknown global ${JSON.stringify(name)}`);
    }
  });

  it('publishes the whitelist the spec names', () => {
    expect(harnessGlobalNames()).toEqual([
      '__agentOverflowTimelineMemoryStats',
      '__agentOverflowTimelineMemoryStatsByPane',
      '__aoMemoryReport',
      '__aoRevealDrain',
      '__paneGeometry',
      '__stickState',
      'uiTrace.recent',
    ]);
  });

  // The drain probe is what a bench and a profile poll to learn when the
  // reveal queue has finished. It is async (main.ts installs it as a
  // dynamic-import stub), so the reader has to await it rather than
  // reporting a Promise as the value.
  it('awaits the reveal-drain probe rather than answering with its promise', async () => {
    (window as { __aoRevealDrain?: () => Promise<unknown> }).__aoRevealDrain = () =>
      Promise.resolve({ v: 1, panes: 2, draining: 1, smoothers: 3, boundaries: 1 });
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'globals',
      name: '__aoRevealDrain',
    })) as { value: unknown };
    expect(result.value).toEqual({ v: 1, panes: 2, draining: 1, smoothers: 3, boundaries: 1 });
  });

  // A page from before the probe existed must read as "no drain info",
  // which the CLI degrades on with a note. An error would fail the run.
  it('answers a page with no drain probe as unavailable, not an error', async () => {
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'globals',
      name: '__aoRevealDrain',
    })) as { unavailable?: true; error?: string };
    expect(result.error).toBeUndefined();
    expect(result.unavailable).toBe(true);
  });
});

// The `open` kind is the only door in this bridge that MUTATES the page. It
// exists because `openThreadInNewPane` has no event channel — the plain
// open rides `notification:activated` and deliberately still does. What is
// asserted here is the refusal surface; the mounting itself needs a real
// pane registry and is covered by the e2e suite.
describe('open kind', () => {
  it('requires a threadId', async () => {
    const result = (await answerHarnessQuery({ v: 1, kind: 'open' })) as ErrorEnvelope;
    expect(result.error).toContain('requires a threadId');
  });

  it('names the page registry when the thread is not in it', async () => {
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'open',
      threadId: 'no-such-thread',
    })) as ErrorEnvelope;
    expect(result.error).toContain('thread registry');
    expect(result.error).toContain('no-such-thread');
  });
});

describe('perf ops', () => {
  it('refuses collect and stop with nothing armed', async () => {
    for (const op of ['collect', 'stop']) {
      const result = (await answerHarnessQuery({ v: 1, kind: 'perf', op })) as ErrorEnvelope;
      expect(result.error).toBe('no perf run is armed');
    }
    const status = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'status' })) as {
      armed: boolean;
    };
    expect(status.armed).toBe(false);
  });

  it('arms, collects and stops, returning a summary over the run', async () => {
    const armed = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start' })) as {
      armed: boolean;
      superseded: unknown;
    };
    expect(armed).toMatchObject({ armed: true, superseded: null });

    const sample = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'collect' })) as {
      v: number;
      domNodes: number;
    };
    expect(sample.v).toBe(1);
    expect(sample.domNodes).toBeGreaterThan(0);

    const summary = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' })) as {
      v: number;
      samples: number;
      meters: string[];
    };
    expect(summary.v).toBe(1);
    // start() takes a seeding sample so a run stopped before its first
    // collect still carries a level.
    expect(summary.samples).toBeGreaterThanOrEqual(2);
    expect(summary.meters).toContain('dom');
  });

  it('hands back a superseded run rather than discarding its numbers', async () => {
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start' });
    const rearmed = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start' })) as {
      superseded: { v: number } | null;
    };
    expect(rearmed.superseded?.v).toBe(1);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' });
  });

  // Unlike a meter NAME, a bad budget cannot narrow the run to nothing, so
  // the spec's list is cleaned rather than refused — and a spec that names
  // none keeps the bridge's own default set.
  it('takes the arm spec budgets, cleaned, and defaults when it names none', async () => {
    await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'start',
      meters: ['busy'],
      budgetsMs: [16, 4, 'nonsense', 0, 4],
    });
    const chosen = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' })) as {
      busy: { budgets: Array<{ budgetMs: number }> };
    };
    expect(chosen.busy.budgets.map((budget) => budget.budgetMs)).toEqual([4, 16]);

    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', meters: ['busy'] });
    const fallback = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' })) as {
      busy: { budgets: Array<{ budgetMs: number }> };
    };
    expect(fallback.busy.budgets.map((budget) => budget.budgetMs)).toEqual([6, 8, 16]);
  });

  it('names an unknown op', async () => {
    const result = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'wat' })) as ErrorEnvelope;
    expect(result.error).toContain('unknown perf op "wat"');
  });

  // A meter name nobody knows filtered to a narrower set and armed anyway,
  // so `--meter fps` answered {armed:true} and then reported zeros for the
  // length of the run. Same rule as the globals whitelist: a typo must
  // read as a typo.
  it('refuses an unknown meter name instead of arming an empty set', async () => {
    const result = (await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'start',
      meters: ['fps', 'frames'],
    })) as ErrorEnvelope;
    expect(result.error).toContain('unknown perf meter "fps"');
    expect(result.error).toContain('layout-shift');
    const status = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'status' })) as {
      armed: boolean;
    };
    expect(status.armed).toBe(false);
  });

  it('echoes the run id it armed, and answers a collect that names it', async () => {
    const armed = (await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'start',
      runId: 'perf-7',
      meters: ['dom'],
    })) as { armed: boolean; runId: string };
    expect(armed).toMatchObject({ armed: true, runId: 'perf-7' });

    const sample = (await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'collect',
      runId: 'perf-7',
    })) as { v: number };
    expect(sample.v).toBe(1);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop', runId: 'perf-7' });
  });

  // Every attached page sees every ui-query and the backend takes the
  // FIRST reply. An unarmed second page answering "no perf run is armed"
  // would win that race and poison the armed page's tick.
  it('says nothing at all about another page\'s run', async () => {
    for (const op of ['collect', 'stop']) {
      const result = await answerHarnessQuery({ v: 1, kind: 'perf', op, runId: 'perf-elsewhere' });
      expect(isHarnessNoReply(result), op).toBe(true);
    }

    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', runId: 'perf-mine', meters: ['dom'] });
    const foreign = await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'collect',
      runId: 'perf-elsewhere',
    });
    expect(isHarnessNoReply(foreign)).toBe(true);

    // An unstamped spec keeps the pre-runId behaviour: this page answers.
    const unstamped = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'collect' })) as {
      v: number;
    };
    expect(unstamped.v).toBe(1);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' });
  });

  // The watchdog exists because an abandoned run keeps the rAF loop firing
  // for the life of the page. A caller that comes back afterwards must
  // learn that, not read a bare "no perf run is armed" and go hunting for
  // a sequencing bug of its own.
  it('tells a late collect that the run self-disarmed', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', meters: ['dom'] });
      vi.advanceTimersByTime(PERF_WATCHDOG_MS);
      for (const op of ['collect', 'stop']) {
        const result = (await answerHarnessQuery({ v: 1, kind: 'perf', op })) as ErrorEnvelope;
        expect(result.error, op).toBe(
          `perf run self-disarmed after ${PERF_WATCHDOG_MS}ms without a collect`,
        );
      }
    } finally {
      vi.useRealTimers();
    }
  });

  // A perf run lives entirely in module state, so a reload takes it with
  // the page. The failure this pins is the SILENT one: a reloaded page that
  // answered the stop would hand back a fresh, empty histogram for a run it
  // never armed, and the report's frontend half would read as measured-and-
  // flat instead of absent. Staying silent is what makes the backend stamp
  // its own FrontendError on the report instead.
  it('says nothing about a run its page reloaded away from', async () => {
    const armed = (await answerHarnessQuery({
      v: 1,
      kind: 'perf',
      op: 'start',
      runId: 'perf-reload',
      meters: ['dom'],
    })) as { armed: boolean; runId: string };
    expect(armed).toMatchObject({ armed: true, runId: 'perf-reload' });

    // The reload: a brand-new module graph, which is exactly the state the
    // page comes back in. The original module is still armed and is what
    // afterEach tears down.
    vi.resetModules();
    const reloaded = await import('./bridge');
    try {
      const stopped = await reloaded.answerHarnessQuery({
        v: 1,
        kind: 'perf',
        op: 'stop',
        runId: 'perf-reload',
      });
      expect(reloaded.isHarnessNoReply(stopped)).toBe(true);

      const collected = await reloaded.answerHarnessQuery({
        v: 1,
        kind: 'perf',
        op: 'collect',
        runId: 'perf-reload',
      });
      expect(reloaded.isHarnessNoReply(collected)).toBe(true);

      // Nothing was resurrected on the way past, either.
      const status = (await reloaded.answerHarnessQuery({
        v: 1,
        kind: 'perf',
        op: 'status',
      })) as { armed: boolean; runId: string };
      expect(status).toMatchObject({ armed: false, runId: '' });
    } finally {
      reloaded.stopHarnessBridge();
    }
  });
});

describe('reload op', () => {
  it('answers before it navigates, and clamps the delay it was handed', async () => {
    const reload = vi.fn();
    const original = window.location;
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { ...original, reload },
    });
    vi.useFakeTimers();
    try {
      const answer = (await answerHarnessQuery({ v: 1, kind: 'reload', delayMs: 99_000 })) as {
        reloading: boolean;
        delayMs: number;
      };
      // The answer is what proves the reply can win the race with the
      // navigation that is about to drop the socket.
      expect(answer).toMatchObject({ reloading: true, delayMs: 5000 });
      expect(reload).not.toHaveBeenCalled();
      vi.advanceTimersByTime(5000);
      expect(reload).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
      Object.defineProperty(window, 'location', { configurable: true, value: original });
    }
  });
});

// The observer is document-wide over childList/characterData/attributes,
// so it allocates a MutationRecord per streaming text delta. Every perf run
// and every bench workload is a measurement of the renderer, so an observer
// that outlives the query that needed it is a probe that invalidates the
// numbers it sits beside. These cases pin its lifetime, not its readings.
describe('mutation clock', () => {
  it('reports a non-negative age even before the observer is installed', () => {
    expect(sinceLastMutationMs()).toBeGreaterThanOrEqual(0);
  });

  it('arms for a settledness query and for nothing else', async () => {
    expect(mutationClockArmed()).toBe(false);

    // Everything that answers without a `settled` field leaves it alone —
    // including the whole perf lifecycle, which is the case that matters:
    // an armed run must measure a renderer with no observer on it unless
    // somebody explicitly asked the page whether it had settled.
    await answerHarnessQuery({ v: 1, kind: 'element', selector: 'body' });
    expect(mutationClockArmed()).toBe(false);
    await answerHarnessQuery({ v: 1, kind: 'globals', name: '__stickState' });
    expect(mutationClockArmed()).toBe(false);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', meters: ['dom'] });
    expect(mutationClockArmed()).toBe(false);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'collect' });
    expect(mutationClockArmed()).toBe(false);
    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' });
    expect(mutationClockArmed()).toBe(false);

    await answerHarnessQuery({ v: 1, kind: 'viewport' });
    expect(mutationClockArmed()).toBe(true);
  });

  it('disarms after the linger and re-arms transparently', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      await answerHarnessQuery({ v: 1, kind: 'viewport' });
      expect(mutationClockArmed()).toBe(true);

      vi.advanceTimersByTime(MUTATION_CLOCK_LINGER_MS - 1);
      expect(mutationClockArmed()).toBe(true);
      vi.advanceTimersByTime(1);
      expect(mutationClockArmed()).toBe(false);

      // Re-armable: the caller does nothing different, it just asks again.
      await answerHarnessQuery({ v: 1, kind: 'viewport' });
      expect(mutationClockArmed()).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  // A settle POLL must observe ONE continuous history. Re-installing the
  // observer per lap would restart the clock every lap, and `settled` could
  // then never become true no matter how quiet the document went.
  it('does not reinstall the observer for a poll inside the linger', async () => {
    const observe = vi.spyOn(MutationObserver.prototype, 'observe');
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      await answerHarnessQuery({ v: 1, kind: 'viewport' });
      expect(observe).toHaveBeenCalledTimes(1);

      for (let lap = 0; lap < 3; lap += 1) {
        vi.advanceTimersByTime(MUTATION_CLOCK_LINGER_MS - 1);
        await answerHarnessQuery({ v: 1, kind: 'viewport' });
      }
      expect(observe).toHaveBeenCalledTimes(1);
      expect(mutationClockArmed()).toBe(true);

      // The poll stops; the linger runs out; the next query pays for a
      // fresh install.
      vi.advanceTimersByTime(MUTATION_CLOCK_LINGER_MS);
      expect(mutationClockArmed()).toBe(false);
      await answerHarnessQuery({ v: 1, kind: 'viewport' });
      expect(observe).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
      observe.mockRestore();
    }
  });

  // Both ends of a run, and both matter for the same reason. Bench setup
  // opens a thread by polling `viewport` and then arms immediately, so a
  // run that inherited the linger would measure its first seconds with an
  // observer on; and a report is the number most damaged by one, so the
  // stop must not leave it running either.
  it('starts and ends a perf run with no observer, whatever preceded it', async () => {
    await answerHarnessQuery({ v: 1, kind: 'viewport' });
    expect(mutationClockArmed()).toBe(true);

    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', meters: ['dom'] });
    expect(mutationClockArmed()).toBe(false);

    // A caller that wants settledness mid-run just asks for it.
    await answerHarnessQuery({ v: 1, kind: 'viewport' });
    expect(mutationClockArmed()).toBe(true);

    await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' });
    expect(mutationClockArmed()).toBe(false);
  });

  // The perf watchdog is the backstop for a caller that vanished mid-run.
  // It still disarms the meters, and the clock is gone by then too — the
  // linger is three orders of magnitude shorter than the watchdog.
  it('leaves no observer behind when the perf watchdog fires', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      await answerHarnessQuery({ v: 1, kind: 'perf', op: 'start', meters: ['dom'] });
      await answerHarnessQuery({ v: 1, kind: 'viewport' });
      expect(mutationClockArmed()).toBe(true);

      vi.advanceTimersByTime(PERF_WATCHDOG_MS);
      const late = (await answerHarnessQuery({ v: 1, kind: 'perf', op: 'stop' })) as ErrorEnvelope;
      expect(late.error).toContain('self-disarmed');
      expect(mutationClockArmed()).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('disarms on teardown', async () => {
    await answerHarnessQuery({ v: 1, kind: 'viewport' });
    expect(mutationClockArmed()).toBe(true);
    stopHarnessBridge();
    expect(mutationClockArmed()).toBe(false);
  });
});
