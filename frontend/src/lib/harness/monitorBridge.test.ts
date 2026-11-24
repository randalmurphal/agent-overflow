import { describe, expect, it, vi } from 'vitest';
import { answerHarnessQuery } from './bridge';
import { dispatchMonitor, stopAllMonitors } from './monitorBridge';

describe('monitor bridge', () => {
  it('lists the versioned monitor catalog without exposing executable fields', async () => {
    const result = await answerHarnessQuery({ v: 1, kind: 'monitor', op: 'list' }) as {
      v: number;
      monitors: Array<Record<string, unknown>>;
    };
    expect(result.v).toBe(1);
    expect(result.monitors).toHaveLength(10);
    expect(result.monitors.map((monitor) => monitor.id)).toContain('semantic-dom-stability');
    expect(result.monitors.some((monitor) => 'create' in monitor)).toBe(false);
  });

  it('refuses a monitor whose capability the page lacks', async () => {
    vi.stubGlobal('PerformanceObserver', undefined);
    const result = await answerHarnessQuery({
      v: 1,
      kind: 'monitor',
      op: 'start',
      runId: 'long-task-run',
      monitorIds: ['long-task'],
    }) as { error?: string };
    vi.unstubAllGlobals();
    expect(result.error).toMatch(/requires unavailable capabilities: performance:longtask/);
  });

  it('starts a browser monitor and returns a run-scoped receipt', async () => {
    const result = await answerHarnessQuery({
      v: 1,
      kind: 'monitor',
      op: 'start',
      runId: 'dom-run',
      monitorIds: ['semantic-dom-stability'],
      atMs: 1,
    }) as { runId?: string; monitors?: Array<{ id: string }> };
    expect(result.runId).toBe('dom-run');
    expect(result.monitors?.map((monitor) => monitor.id)).toEqual(['semantic-dom-stability']);
    const stopped = await answerHarnessQuery({ v: 1, kind: 'monitor', op: 'stop', runId: 'dom-run', atMs: 2 }) as { status?: string; monitors?: unknown[] };
    expect(stopped.status).toBe('complete');
    expect(stopped.monitors).toHaveLength(1);
    const last = await answerHarnessQuery({ v: 1, kind: 'monitor', op: 'last', runId: 'dom-run', atMs: 2 }) as { runs?: unknown[] };
    expect(last.runs).toHaveLength(1);
  });

  it('accepts an explicit collect operation for a live monitor', async () => {
    await answerHarnessQuery({
      v: 1, kind: 'monitor', op: 'start', runId: 'collect-run', monitorIds: ['semantic-dom-stability'], atMs: 10,
    });
    const collected = await answerHarnessQuery({ v: 1, kind: 'monitor', op: 'collect', runId: 'collect-run', atMs: 11 }) as { runId?: string; monitors?: unknown[] };
    expect(collected.runId).toBe('collect-run');
    expect(collected.monitors).toHaveLength(1);
    await answerHarnessQuery({ v: 1, kind: 'monitor', op: 'stop', runId: 'collect-run', atMs: 12 });
  });

  it('retains partial stop evidence when the bridge unloads', () => {
    dispatchMonitor({ v: 1, kind: 'monitor', op: 'start', runId: 'unload-run', monitorIds: ['semantic-dom-stability'], atMs: 20 });
    const stopped = stopAllMonitors(21);
    expect(stopped).toHaveLength(1);
    expect(dispatchMonitor({ v: 1, kind: 'monitor', op: 'last', runId: 'unload-run' })).toMatchObject({ runs: stopped });
  });

  it('refuses malformed monitor timestamps and timeouts', async () => {
    await expect(answerHarnessQuery({
      v: 1, kind: 'monitor', op: 'start', runId: 'bad-time', monitorIds: ['semantic-dom-stability'], atMs: 'now',
    })).resolves.toMatchObject({ error: expect.stringContaining('atMs') });
    await expect(answerHarnessQuery({
      v: 1, kind: 'monitor', op: 'start', runId: 'bad-timeout', monitorIds: ['semantic-dom-stability'], heartbeatTimeoutMs: 0,
    })).resolves.toMatchObject({ error: expect.stringContaining('positive') });
  });
});
