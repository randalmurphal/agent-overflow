// Dispatch rules: the union's version gate, the error envelope, and the
// globals whitelist. What is NOT here is anything timing-shaped (the rAF
// meter, the mutation clock's own cadence) — those need a real engine and
// are covered by e2e/tests/harness-bridge.spec.ts.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { answerHarnessQuery, isHarnessNoReply, sinceLastMutationMs, stopHarnessBridge } from './bridge';
import { harnessGlobalNames } from './globals';
import { PERF_WATCHDOG_MS } from './perf';

interface ErrorEnvelope {
  error?: string;
}

afterEach(() => {
  stopHarnessBridge();
  delete (window as { __stickState?: unknown }).__stickState;
  delete (window as { __agentOverflowUiTrace?: unknown }).__agentOverflowUiTrace;
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
      '__paneGeometry',
      '__stickState',
      'uiTrace.recent',
    ]);
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

describe('mutation clock', () => {
  it('reports a non-negative age even before the observer is installed', () => {
    expect(sinceLastMutationMs()).toBeGreaterThanOrEqual(0);
  });
});
