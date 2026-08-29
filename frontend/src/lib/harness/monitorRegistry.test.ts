import { describe, expect, it } from 'vitest';
import { MonitorRegistry } from './monitorRegistry';
import { MonitorRefusal, type CustomMonitorSpec, type MonitorInstance } from './monitorTypes';

const frameSpec: CustomMonitorSpec<MonitorInstance> = {
  v: 1,
  id: 'test-frame',
  title: 'Test frame',
  description: 'A test monitor.',
  compatibilityLeg: 'instrumented-renderer',
  perturbation: 'none',
  requires: ['animation-frame'],
  custom: true,
  create: (context) => {
    const state = { starts: 0, overlaps: [] as string[], stops: 0 };
    return {
      start: () => {
        state.starts += 1;
        context.observe({ runId: context.runId });
      },
      overlap: (runId) => state.overlaps.push(runId),
      stop: () => {
        state.stops += 1;
        return state;
      },
    };
  },
};

function registry(): MonitorRegistry {
  return new MonitorRegistry([frameSpec]);
}

describe('MonitorRegistry', () => {
	it('rolls back overlap records when a host fails during creation', () => {
		const failing: CustomMonitorSpec<MonitorInstance> = {
			...frameSpec,
			id: 'test-failing',
			create: () => { throw new Error('host failed'); },
		};
		const monitors = new MonitorRegistry([frameSpec, failing]);
		const capabilities = new Set(['animation-frame'] as const);
		monitors.start({ runId: 'existing', monitorIds: ['test-frame'], capabilities, atMs: 10 });
		expect(() => monitors.start({ runId: 'broken', monitorIds: ['test-failing'], capabilities, atMs: 20 })).toThrow(/host failed/);
		expect(monitors.activeRunIds()).toEqual(['existing']);
		const result = monitors.stop('existing', 30);
		expect(result.overlap).toEqual([]);
	});

  it('refuses unavailable capabilities before starting a run', () => {
    const monitors = registry();
    expect(() => monitors.start({ monitorIds: ['test-frame'], capabilities: new Set(), atMs: 10 }))
      .toThrow(MonitorRefusal);
    expect(monitors.activeRunIds()).toEqual([]);
  });

  it('records lifecycle, explicit overlap, and custom observations', () => {
    const monitors = registry();
    const capabilities = new Set(['animation-frame'] as const);
    const first = monitors.start({ runId: 'run-a', monitorIds: ['test-frame'], capabilities, atMs: 10 });
    expect(first.overlap).toEqual([]);
    monitors.heartbeat('run-a', 20);
    const second = monitors.start({ runId: 'run-b', monitorIds: ['test-frame'], capabilities, atMs: 30 });
    expect(second.overlap).toEqual([{ runId: 'run-b', withRunId: 'run-a', atMs: 30 }]);
    monitors.overlap('run-a', 'run-b', 35);
    monitors.heartbeat('run-a', 40);
    const result = monitors.stop('run-a', 45);
    expect(result.status).toBe('complete');
    expect(result.heartbeats).toBe(2);
    expect(result.overlap).toEqual([
      { runId: 'run-a', withRunId: 'run-b', atMs: 30 },
      { runId: 'run-a', withRunId: 'run-b', atMs: 35 },
    ]);
    expect(result.monitors[0]?.observations).toEqual([
      { atMs: 10, value: { runId: 'run-a' } },
      { atMs: 45, value: { starts: 1, overlaps: ['run-b'], stops: 1 } },
    ]);
    expect(monitors.activeRunIds()).toEqual(['run-b']);
    expect(() => monitors.start({ runId: 'run-b', monitorIds: ['test-frame'], capabilities, atMs: 50 }))
      .toThrow(/already active/);
  });

  it('returns a partial result when a heartbeat gap exceeds the declared bound', () => {
    const monitors = registry();
    const capabilities = new Set(['animation-frame'] as const);
    monitors.start({ runId: 'run', monitorIds: ['test-frame'], capabilities, heartbeatTimeoutMs: 5, atMs: 10 });
    const result = monitors.stop('run', 16);
    expect(result.status).toBe('partial');
    expect(result.errors[0]).toMatch(/heartbeat gap 6ms exceeded 5ms/);
  });

  it('rejects duplicate registrations and duplicate monitor IDs', () => {
    const monitors = registry();
    expect(() => monitors.register(frameSpec)).toThrow(/already registered/);
    expect(() => monitors.start({
      runId: 'run',
      monitorIds: ['test-frame', 'test-frame'],
      capabilities: new Set(['animation-frame']),
      atMs: 1,
    })).toThrow(/listed more than once/);
  });

  it('enforces the selected compatibility leg before arming hosts', () => {
    const monitors = registry();
    expect(() => monitors.start({
      runId: 'run',
      monitorIds: ['test-frame'],
      compatibilityLeg: 'clean-renderer',
      capabilities: new Set(['animation-frame']),
      atMs: 1,
    })).toThrow(/belongs to instrumented-renderer/);
  });

  it('collects explicit host values and keeps the stopped receipt available', () => {
    const spec: CustomMonitorSpec<MonitorInstance> = {
      ...frameSpec,
      id: 'test-collect',
      create: () => ({ collect: (atMs) => ({ atMs, sample: 'ok' }) }),
    };
    const monitors = new MonitorRegistry([spec]);
    const capabilities = new Set(['animation-frame'] as const);
    monitors.start({ runId: 'collect-run', monitorIds: ['test-collect'], capabilities, atMs: 10 });
    const collected = monitors.collect('collect-run', 20);
    expect(collected.partial).toBe(false);
    expect(collected.monitors[0]?.observations).toEqual([{ atMs: 20, value: { atMs: 20, sample: 'ok' } }]);
    const stopped = monitors.stop('collect-run', 30);
    expect(monitors.lastStopped('collect-run').runs).toEqual([stopped]);
  });

  it('rejects oversized and deeply nested observations without retaining them', () => {
    const spec: CustomMonitorSpec<MonitorInstance> = {
      ...frameSpec,
      id: 'test-bounds',
      create: (context) => ({
        start: () => context.observe('x'.repeat(20 * 1024)),
        collect: () => {
          let value: unknown = 'leaf';
          for (let i = 0; i < 10; i += 1) value = { value };
          return value;
        },
      }),
    };
    const monitors = new MonitorRegistry([spec]);
    const capabilities = new Set(['animation-frame'] as const);
    monitors.start({ runId: 'bounds-run', monitorIds: ['test-bounds'], capabilities, atMs: 1 });
    const collected = monitors.collect('bounds-run', 2);
    expect(collected.partial).toBe(true);
    expect(collected.monitors[0]?.observations).toEqual([]);
    expect(collected.monitors[0]?.errors.join('\n')).toMatch(/exceeds/);
    expect(monitors.stop('bounds-run', 3).status).toBe('partial');
  });
});
