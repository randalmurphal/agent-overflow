import type { CompatibilityLeg, MonitorId } from './monitor-catalog.ts';

export const FUNCTIONAL_FLOW_VERSION = 1 as const;
export const MAX_FLOW_WAIT_MS = 60_000;
export const MAX_FLOW_SAMPLES = 256;

export type SemanticTarget =
  | { testId: string }
  | { role: string; name?: string; exact?: boolean }
  | { label: string; exact?: boolean }
  | { text: string; exact?: boolean }
  | { placeholder: string; exact?: boolean };

export type FlowAction =
  | { kind: 'click' | 'focus' | 'approve'; target: SemanticTarget }
  | { kind: 'fill'; target: SemanticTarget; value: string }
  | { kind: 'type'; target: SemanticTarget; text: string; delayMs?: number }
  | { kind: 'key'; target: SemanticTarget; key: string }
  | { kind: 'drag'; source: SemanticTarget; target: SemanticTarget }
  | { kind: 'wheel'; target: SemanticTarget; deltaX?: number; deltaY: number }
  | { kind: 'viewport'; width: number; height: number };

export type FlowAssertion =
  | { kind: 'visible' | 'hidden' | 'checked' | 'disabled' | 'focused' | 'selected'; target: SemanticTarget; expected?: boolean }
  | { kind: 'text' | 'value'; target: SemanticTarget; expected: string }
  | { kind: 'count'; target: SemanticTarget; expected: number }
  | { kind: 'attribute'; target: SemanticTarget; name: string; expected: string | null };

export interface FlowMonitor {
  id: string;
  /** Registry monitor ID. Omitted keeps the legacy semantic target monitor. */
  monitorId?: MonitorId;
  target?: SemanticTarget;
  durationMs: number;
  intervalMs?: number;
  compatibilityLeg?: CompatibilityLeg;
}

export interface FunctionalScenario {
  v: typeof FUNCTIONAL_FLOW_VERSION;
  id: string;
  actions: FlowAction[];
  assertions?: FlowAssertion[];
  monitors?: FlowMonitor[];
  extensions?: FlowExtensionInvocation[];
}

export interface FlowExtensionInvocation {
  extension: string;
  operation: string;
  input?: unknown;
}

export interface SemanticObservation {
  count: number;
  visible: boolean;
  text: string;
  value: string | null;
  checked: boolean | null;
  disabled: boolean;
  focused: boolean;
  selected: boolean | null;
  attributes: Record<string, string | null>;
  timestamp: number;
}

export interface FlowMonitorSample extends SemanticObservation {
  monitorId: string;
  runId: string;
  heartbeat: number;
}

export interface FlowTypedMonitorResult {
  v: number;
  runId: string;
  status: 'complete' | 'partial' | 'failed';
  startedAtMs: number;
  stoppedAtMs: number;
  heartbeats: number;
  overlap: readonly { runId: string; withRunId: string; atMs: number }[];
  monitors: readonly {
    monitorId: string;
    status: 'complete' | 'partial' | 'failed';
    startedAtMs: number;
    stoppedAtMs: number;
    heartbeats: number;
    lastHeartbeatAtMs: number;
    observations: readonly { atMs: number; value: unknown }[];
    errors: readonly string[];
  }[];
  errors: readonly string[];
}

export interface FlowMonitorResult {
  monitorId: string;
  runId: string;
  compatibilityLeg?: CompatibilityLeg;
  status: 'complete' | 'partial';
  startedAtMs: number;
  stoppedAtMs: number;
  heartbeats: number;
  observations: number;
  observationDrops?: number;
  error?: string;
  /** The actual page-side MonitorRegistry result for a typed monitor. */
  typed?: FlowTypedMonitorResult;
}

export interface FunctionalFlowReport {
  v: typeof FUNCTIONAL_FLOW_VERSION;
  runId: string;
  scenario: string;
  observations: FlowMonitorSample[];
  monitors: FlowMonitorResult[];
  overlaps: Array<{ runId: string; withRunId: string; atMs: number }>;
  lastObservations: Record<string, SemanticObservation>;
}

export interface FlowExtensionContext {
  ui: SemanticUI;
  observe(target: SemanticTarget): Promise<SemanticObservation>;
}

export interface FunctionalFlowExtension<I = unknown> {
  readonly name: string;
  readonly actions?: Record<string, (context: FlowExtensionContext, input: I) => Promise<void>>;
  readonly assertions?: Record<string, (context: FlowExtensionContext, input: I) => Promise<void>>;
}

// Imported here only for the extension context's public type shape. The
// implementation lives in flow-ui.ts and remains outside the model layer.
import type { SemanticUI } from './flow-ui.ts';
