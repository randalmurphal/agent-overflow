import { validateMonitorSelection, validateCompatibilityLeg, type CompatibilityLeg, type MonitorId } from './monitor-catalog.ts';
import {
  FUNCTIONAL_FLOW_VERSION,
  MAX_FLOW_WAIT_MS,
  type FlowAction,
  type FlowAssertion,
  type FlowExtensionInvocation,
  type FlowMonitor,
  type FunctionalScenario,
  type SemanticTarget,
} from './flow-model.ts';

const targetKeys = ['testId', 'role', 'name', 'exact', 'label', 'text', 'placeholder'] as const;
const actionKeys: Record<string, readonly string[]> = {
  click: ['kind', 'target'], focus: ['kind', 'target'], approve: ['kind', 'target'],
  fill: ['kind', 'target', 'value'], type: ['kind', 'target', 'text', 'delayMs'],
  key: ['kind', 'target', 'key'], drag: ['kind', 'source', 'target'],
  wheel: ['kind', 'target', 'deltaX', 'deltaY'], viewport: ['kind', 'width', 'height'],
};
const assertionKeys: Record<string, readonly string[]> = {
  visible: ['kind', 'target', 'expected'], hidden: ['kind', 'target', 'expected'],
  checked: ['kind', 'target', 'expected'], disabled: ['kind', 'target', 'expected'],
  focused: ['kind', 'target', 'expected'], selected: ['kind', 'target', 'expected'],
  text: ['kind', 'target', 'expected'], value: ['kind', 'target', 'expected'],
  count: ['kind', 'target', 'expected'], attribute: ['kind', 'target', 'name', 'expected'],
};

function record(value: unknown, path: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${path} must be an object`);
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, allowed: readonly string[], path: string): void {
  for (const key of Object.keys(value)) if (!allowed.includes(key)) throw new Error(`${path}.${key} is not supported`);
}

function requiredString(value: Record<string, unknown>, key: string, path: string): string {
  if (typeof value[key] !== 'string' || value[key] === '') throw new Error(`${path}.${key} must be a non-empty string`);
  return value[key] as string;
}

function finiteNumber(value: Record<string, unknown>, key: string, path: string): number {
  if (typeof value[key] !== 'number' || !Number.isFinite(value[key])) throw new Error(`${path}.${key} must be finite`);
  return value[key] as number;
}

export function parseSemanticTarget(raw: unknown, path = 'target'): SemanticTarget {
  const value = record(raw, path);
  exactKeys(value, targetKeys, path);
  const present = ['testId', 'role', 'label', 'text', 'placeholder'].filter((key) => key in value);
  if (present.length !== 1) throw new Error(`${path} must select exactly one semantic strategy`);
  const strategy = present[0]!;
  const result: Record<string, unknown> = { [strategy]: requiredString(value, strategy, path) };
  if ('name' in value) result.name = requiredString(value, 'name', path);
  if ('exact' in value && typeof value.exact !== 'boolean') throw new Error(`${path}.exact must be boolean`);
  if ('exact' in value) result.exact = value.exact;
	if (strategy !== 'role' && 'name' in value) throw new Error(`${path}.name requires role`);
  return result as SemanticTarget;
}

function parseAction(raw: unknown, index: number): FlowAction {
  const path = `actions[${index}]`;
  const value = record(raw, path);
  const kind = requiredString(value, 'kind', path);
  const allowed = actionKeys[kind];
  if (!allowed) throw new Error(`${path}.kind ${JSON.stringify(kind)} is unsupported`);
  exactKeys(value, allowed, path);
  if (kind === 'viewport') {
    const width = finiteNumber(value, 'width', path); const height = finiteNumber(value, 'height', path);
    if (width <= 0 || height <= 0) throw new Error(`${path} viewport dimensions must be positive`);
    return { kind, width, height };
  }
  if (kind === 'drag') return { kind, source: parseSemanticTarget(value.source, `${path}.source`), target: parseSemanticTarget(value.target, `${path}.target`) };
  const target = parseSemanticTarget(value.target, `${path}.target`);
  if (kind === 'fill') return { kind, target, value: requiredString(value, 'value', path) };
  if (kind === 'type') return { kind, target, text: requiredString(value, 'text', path), ...(value.delayMs === undefined ? {} : { delayMs: finiteNumber(value, 'delayMs', path) }) };
  if (kind === 'key') return { kind, target, key: requiredString(value, 'key', path) };
  if (kind === 'wheel') {
    const deltaY = finiteNumber(value, 'deltaY', path);
    return { kind, target, deltaY, ...(value.deltaX === undefined ? {} : { deltaX: finiteNumber(value, 'deltaX', path) }) };
  }
  return { kind, target } as FlowAction;
}

function parseAssertion(raw: unknown, index: number): FlowAssertion {
  const path = `assertions[${index}]`; const value = record(raw, path);
  const kind = requiredString(value, 'kind', path); const allowed = assertionKeys[kind];
  if (!allowed) throw new Error(`${path}.kind ${JSON.stringify(kind)} is unsupported`);
  exactKeys(value, allowed, path);
  const target = parseSemanticTarget(value.target, `${path}.target`);
  if (kind === 'count') { const expected = finiteNumber(value, 'expected', path); if (!Number.isInteger(expected) || expected < 0) throw new Error(`${path}.expected must be a non-negative integer`); return { kind, target, expected }; }
  if (kind === 'attribute') return { kind, target, name: requiredString(value, 'name', path), expected: value.expected === null ? null : requiredString(value, 'expected', path) };
  if (kind === 'visible' || kind === 'hidden' || kind === 'checked' || kind === 'disabled' || kind === 'focused' || kind === 'selected') {
    const expected = value.expected === undefined ? true : value.expected;
    if (typeof expected !== 'boolean') throw new Error(`${path}.expected must be boolean`);
    return { kind, target, expected };
  }
  return { kind, target, expected: requiredString(value, 'expected', path) } as FlowAssertion;
}

export function parseFunctionalScenario(raw: unknown): FunctionalScenario {
  const value = record(raw, 'scenario');
  exactKeys(value, ['v', 'id', 'actions', 'assertions', 'monitors', 'extensions'], 'scenario');
  if (value.v !== FUNCTIONAL_FLOW_VERSION) throw new Error(`scenario.v must be ${FUNCTIONAL_FLOW_VERSION}`);
  const id = requiredString(value, 'id', 'scenario');
  if (!Array.isArray(value.actions)) throw new Error('scenario.actions must be an array');
  const actions = value.actions.map(parseAction);
  const assertions = value.assertions === undefined ? undefined : Array.isArray(value.assertions) ? value.assertions.map(parseAssertion) : (() => { throw new Error('scenario.assertions must be an array'); })();
  const monitors = value.monitors === undefined ? undefined : parseMonitors(value.monitors);
  const extensions = value.extensions === undefined ? undefined : parseExtensions(value.extensions);
  return { v: FUNCTIONAL_FLOW_VERSION, id, actions, ...(assertions ? { assertions } : {}), ...(monitors ? { monitors } : {}), ...(extensions ? { extensions } : {}) };
}

function parseMonitors(raw: unknown): FlowMonitor[] {
  if (!Array.isArray(raw)) throw new Error('scenario.monitors must be an array');
  const seen = new Set<string>();
  return raw.map((entry, index) => {
    const path = `monitors[${index}]`;
    const value = record(entry, path);
    exactKeys(value, ['id', 'monitorId', 'target', 'durationMs', 'intervalMs', 'compatibilityLeg'], path);
    const durationMs = finiteNumber(value, 'durationMs', path);
    if (durationMs <= 0 || durationMs > MAX_FLOW_WAIT_MS) throw new Error(`${path}.durationMs must be between 1 and ${MAX_FLOW_WAIT_MS}`);
    const intervalMs = value.intervalMs === undefined ? undefined : finiteNumber(value, 'intervalMs', path);
    if (intervalMs !== undefined && (intervalMs <= 0 || intervalMs > MAX_FLOW_WAIT_MS)) throw new Error(`${path}.intervalMs must be between 1 and ${MAX_FLOW_WAIT_MS}`);
    const id = requiredString(value, 'id', path);
    const monitorId = value.monitorId === undefined ? undefined : requiredString(value, 'monitorId', path) as MonitorId;
    if (seen.has(monitorId ?? id)) throw new Error(`${path} duplicates monitor ${JSON.stringify(monitorId ?? id)}`);
    seen.add(monitorId ?? id);
    if (monitorId) {
      if (value.target !== undefined) throw new Error(`${path}.target is not allowed for a typed monitor`);
      if (value.compatibilityLeg === undefined) throw new Error(`${path}.compatibilityLeg is required for a typed monitor`);
      const compatibilityLeg = validateCompatibilityLeg(requiredString(value, 'compatibilityLeg', path));
      validateMonitorSelection(monitorId, compatibilityLeg);
      return { id, monitorId, durationMs, ...(intervalMs === undefined ? {} : { intervalMs }), compatibilityLeg };
    }
    if (value.target === undefined) throw new Error(`${path}.target is required for a semantic monitor`);
    if (value.compatibilityLeg !== undefined) throw new Error(`${path}.compatibilityLeg requires an explicit monitorId`);
    return { id, target: parseSemanticTarget(value.target, `${path}.target`), durationMs, ...(intervalMs === undefined ? {} : { intervalMs }) };
  });
}

function parseExtensions(raw: unknown): FlowExtensionInvocation[] {
  if (!Array.isArray(raw)) throw new Error('scenario.extensions must be an array');
  return raw.map((entry, index) => { const path = `extensions[${index}]`; const value = record(entry, path); exactKeys(value, ['extension', 'operation', 'input'], path); return { extension: requiredString(value, 'extension', path), operation: requiredString(value, 'operation', path), ...(value.input === undefined ? {} : { input: value.input }) }; });
}
