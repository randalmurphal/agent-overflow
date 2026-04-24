import type { RuntimeMode, Thread } from '../types/models';

const STORAGE_KEY = 'agent-overflow:runtime-mode-drafts:v1';
const MAX_DRAFTS = 100;

const RUNTIME_MODES = new Set<RuntimeMode>([
  'approval-required',
  'auto-accept-edits',
  'full-access',
]);

let overrides: Record<string, RuntimeMode> = $state(loadOverrides());

function isRuntimeMode(value: unknown): value is RuntimeMode {
  return typeof value === 'string' && RUNTIME_MODES.has(value as RuntimeMode);
}

function normalizeRuntimeMode(value: unknown): RuntimeMode | null {
  return isRuntimeMode(value) ? value : null;
}

function loadOverrides(): Record<string, RuntimeMode> {
  if (typeof localStorage === 'undefined') return {};
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    const next: Record<string, RuntimeMode> = {};
    for (const [threadId, mode] of Object.entries(parsed)) {
      if (isRuntimeMode(mode)) next[threadId] = mode;
    }
    const entries = Object.entries(next);
    if (entries.length <= MAX_DRAFTS) return next;
    const trimmed = Object.fromEntries(entries.slice(entries.length - MAX_DRAFTS)) as Record<
      string,
      RuntimeMode
    >;
    localStorage.setItem(STORAGE_KEY, JSON.stringify(trimmed));
    return trimmed;
  } catch (error) {
    console.warn(`Failed to load runtime mode drafts from ${STORAGE_KEY}`, error);
    try {
      localStorage.removeItem(STORAGE_KEY);
    } catch (removeError) {
      console.warn(`Failed to clear corrupt runtime mode drafts from ${STORAGE_KEY}`, removeError);
    }
    return {};
  }
}

function persistOverrides(): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(overrides));
  } catch (error) {
    console.warn('Failed to persist runtime mode draft', error);
  }
}

export function runtimeModeForThread(thread: Thread | null | undefined): RuntimeMode {
  if (!thread) return 'full-access';
  return overrides[thread.id] ?? normalizeRuntimeMode(thread.runtimeMode) ?? 'full-access';
}

export function hasRuntimeModeDraft(thread: Thread | null | undefined): boolean {
  if (!thread) return false;
  const override = overrides[thread.id];
  if (override === undefined) return false;
  return override !== normalizeRuntimeMode(thread.runtimeMode);
}

export function setRuntimeModeDraft(threadId: string, mode: RuntimeMode): void {
  const next = { ...overrides, [threadId]: mode };
  const entries = Object.entries(next);
  overrides = Object.fromEntries(entries.slice(Math.max(0, entries.length - MAX_DRAFTS))) as Record<
    string,
    RuntimeMode
  >;
  persistOverrides();
}

export function clearRuntimeModeDraft(threadId: string): void {
  if (overrides[threadId] === undefined) return;
  const next = { ...overrides };
  delete next[threadId];
  overrides = next;
  persistOverrides();
}

export function resetRuntimeModeDraftsForTest(): void {
  overrides = {};
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch (error) {
    console.warn(`Failed to reset runtime mode drafts from ${STORAGE_KEY}`, error);
  }
}
