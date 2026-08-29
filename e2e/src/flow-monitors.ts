import { validateMonitorSelection, type CompatibilityLeg } from './monitor-catalog.ts';
import type { FlowMonitor, FlowMonitorResult, FlowMonitorSample, FlowTypedMonitorResult } from './flow-model.ts';
import { MAX_FLOW_SAMPLES } from './flow-model.ts';
import { SemanticUI } from './flow-ui.ts';

export interface FlowMonitorHost {
  readonly ui: SemanticUI;
  readonly compatibilityLeg?: CompatibilityLeg;
  readonly monitorQuery?: (spec: Record<string, unknown>) => Promise<unknown>;
}

export async function runFlowMonitor(host: FlowMonitorHost, monitor: FlowMonitor, output: FlowMonitorSample[], results: FlowMonitorResult[], runId: string, signal: AbortSignal): Promise<void> {
  const interval = monitor.intervalMs ?? 100;
  const started = Date.now();
  if (monitor.monitorId) validateMonitorSelection(monitor.monitorId, host.compatibilityLeg);
  const monitorRunId = `${runId}/${monitor.monitorId ?? monitor.id}`;
  if (monitor.monitorId) {
    if (!host.monitorQuery) throw new Error(`typed monitor ${JSON.stringify(monitor.monitorId)} requires a monitor query client`);
    await typedMonitor(host.monitorQuery, monitor, results, monitorRunId, signal);
    return;
  }
  let heartbeats = 0;
  let observationDrops = 0;
  let observations = 0;
  let resultRecorded = false;
  const recordCancellation = (): void => {
    if (resultRecorded) return;
    results.push({
      monitorId: monitor.monitorId ?? monitor.id,
      runId: monitorRunId,
      compatibilityLeg: monitor.compatibilityLeg,
      status: 'partial',
      startedAtMs: started,
      stoppedAtMs: Date.now(),
      heartbeats,
      observations,
      ...(observationDrops > 0 ? { observationDrops } : {}),
      error: `monitor cancelled before its ${monitor.durationMs}ms duration completed`,
    });
    resultRecorded = true;
  };
  if (!monitor.target) {
    const error = `monitor ${JSON.stringify(monitor.monitorId)} has no semantic target for this flow host`;
    results.push({ monitorId: monitor.monitorId ?? monitor.id, runId: monitorRunId, compatibilityLeg: monitor.compatibilityLeg, status: 'partial', startedAtMs: started, stoppedAtMs: Date.now(), heartbeats: 0, observations: 0, error });
    throw new Error(error);
  }
  try {
    while (!signal.aborted) {
      heartbeats += 1;
      const observation = await host.ui.observe(monitor.target);
      if (signal.aborted) {
        recordCancellation();
        return;
      }
      if (observations < MAX_FLOW_SAMPLES) {
        output.push({ monitorId: monitor.monitorId ?? monitor.id, runId: monitorRunId, heartbeat: heartbeats, ...observation });
        observations += 1;
      } else {
        observationDrops += 1;
      }
      if (heartbeats > 1 && Date.now() - started >= monitor.durationMs) break;
      await waitForFlowSignal(Math.min(interval, Math.max(1, started + monitor.durationMs - Date.now())), signal);
    }
    if (signal.aborted) {
      recordCancellation();
      return;
    }
    results.push({ monitorId: monitor.monitorId ?? monitor.id, runId: monitorRunId, compatibilityLeg: monitor.compatibilityLeg, status: observationDrops > 0 ? 'partial' : 'complete', startedAtMs: started, stoppedAtMs: Date.now(), heartbeats, observations, ...(observationDrops > 0 ? { observationDrops, error: `observation limit reached; dropped ${observationDrops} samples` } : {}) });
    resultRecorded = true;
    if (observationDrops > 0) throw new Error(`monitor ${JSON.stringify(monitor.monitorId ?? monitor.id)} exceeded the ${MAX_FLOW_SAMPLES}-sample limit`);
  } catch (error) {
    if (signal.aborted) {
      recordCancellation();
      return;
    }
    if (!resultRecorded) results.push({ monitorId: monitor.monitorId ?? monitor.id, runId: monitorRunId, compatibilityLeg: monitor.compatibilityLeg, status: 'partial', startedAtMs: started, stoppedAtMs: Date.now(), heartbeats, observations, ...(observationDrops > 0 ? { observationDrops } : {}), error: String(error) });
    throw error;
  }
}

async function typedMonitor(query: (spec: Record<string, unknown>) => Promise<unknown>, monitor: FlowMonitor, results: FlowMonitorResult[], monitorRunId: string, signal: AbortSignal): Promise<void> {
  let started = false;
  let heartbeats = 0;
  let failure: unknown;
  let stopped: Record<string, unknown> | undefined;
  let completedDuration = false;
  const startedAt = Date.now();
  try {
    assertMonitorQuery(await query({
      v: 1,
      kind: 'monitor',
      op: 'start',
      runId: monitorRunId,
      monitorIds: [monitor.monitorId!],
      ...(monitor.compatibilityLeg === undefined ? {} : { compatibilityLeg: monitor.compatibilityLeg }),
    }), 'start', monitorRunId);
    started = true;
    while (!signal.aborted) {
      if (heartbeats > 0) await waitForFlowSignal(Math.min(monitor.intervalMs ?? 100, Math.max(1, startedAt + monitor.durationMs - Date.now())), signal);
      if (signal.aborted) break;
      assertMonitorQuery(await query({ v: 1, kind: 'monitor', op: 'heartbeat', runId: monitorRunId }), 'heartbeat', monitorRunId);
      heartbeats += 1;
      if (heartbeats > 1 && Date.now() - startedAt >= monitor.durationMs) {
        completedDuration = true;
        break;
      }
    }
  } catch (error) {
    failure = error;
  } finally {
    if (started) {
      try {
        stopped = assertMonitorQuery(await query({ v: 1, kind: 'monitor', op: 'stop', runId: monitorRunId }), 'stop', monitorRunId);
      } catch (error) {
        failure ??= error;
      }
    }
  }
  const typedStatus = typeof stopped?.status === 'string' ? stopped.status : 'partial';
  const typedMonitors = Array.isArray(stopped?.monitors) ? stopped.monitors : [];
  const typedMonitorResult = typedMonitors[0] as Record<string, unknown> | undefined;
  results.push({
    monitorId: monitor.monitorId!,
    runId: monitorRunId,
    compatibilityLeg: monitor.compatibilityLeg,
    status: typedStatus === 'complete' && !failure && completedDuration ? 'complete' : 'partial',
    startedAtMs: typeof stopped?.startedAtMs === 'number' ? stopped.startedAtMs : startedAt,
    stoppedAtMs: typeof stopped?.stoppedAtMs === 'number' ? stopped.stoppedAtMs : Date.now(),
    heartbeats: typeof stopped?.heartbeats === 'number' ? stopped.heartbeats : heartbeats,
    observations: Array.isArray(typedMonitorResult?.observations) ? typedMonitorResult.observations.length : 0,
    ...(stopped ? { typed: stopped as unknown as FlowTypedMonitorResult } : {}),
    ...(failure ? { error: String(failure) } : {}),
  });
  if (failure) throw failure;
  if (!completedDuration) throw new Error(`typed monitor ${JSON.stringify(monitor.monitorId)} was cancelled before its ${monitor.durationMs}ms duration completed`);
  if (typedStatus !== 'complete') throw new Error(`typed monitor ${JSON.stringify(monitor.monitorId)} returned status ${JSON.stringify(typedStatus)}`);
}

function assertMonitorQuery(value: unknown, operation: string, runId: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`monitor ${operation} ${JSON.stringify(runId)} returned a non-object result`);
  const result = value as Record<string, unknown>;
  if (typeof result.error === 'string' && result.error !== '') throw new Error(`monitor ${operation} ${JSON.stringify(runId)} failed: ${result.error}`);
  return result;
}

export function waitForFlowSignal(delayMs: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const finish = () => {
      if (timer !== undefined) clearTimeout(timer);
      signal.removeEventListener('abort', finish);
      resolve();
    };
    if (signal.aborted) {
      finish();
      return;
    }
    timer = setTimeout(finish, delayMs);
    signal.addEventListener('abort', finish, { once: true });
  });
}
