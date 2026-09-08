// Frontend-only choices; a thread focus change never retargets telemetry.
import { getAttachedBackends, backendDisplayName, backendReachable } from './attachedBackends.svelte';
import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';
import { onBackendIdentity } from '../transport/backendIdentity';
import type { BackendKey } from '../transport/backendKey';

export type TelemetryKind = 'usage' | 'system';
const keys = { usage: 'usage-computers', system: 'system-stats-computers' };
function read(kind: TelemetryKind): string[] | null {
  const raw = readFrontendValue(keys[kind]);
  return Array.isArray(raw) && raw.length > 0 && raw.length <= 256 && raw.every((id) => typeof id === 'string' && id.length <= 128)
    ? [...new Set(raw)] : null;
}
let usage = $state.raw(read('usage'));
let system = $state.raw(read('system'));
let identityRevision = $state(0);
onBackendIdentity(() => { identityRevision++; });
onFrontendValueChanged(keys.usage, () => { usage = read('usage'); });
onFrontendValueChanged(keys.system, () => { system = read('system'); });

export function telemetrySelection(kind: TelemetryKind): readonly string[] | null { return kind === 'usage' ? usage : system; }
export function setTelemetrySelection(kind: TelemetryKind, next: readonly string[] | null): void {
  if (next?.length === 0) return;
  const computers = getAttachedBackends();
  const value = next === null ? null : [...new Set(next.map((id) => {
    const computer = computers.find((entry) => entry.id === id);
    return computer ? computer.backendId || (computer.home ? '@local' : computer.id) : id;
  }))];
  if (!writeFrontendValue(keys[kind], value)) return;
  if (kind === 'usage') usage = value;
  else system = value;
}

export function telemetryComputers(kind: TelemetryKind) {
  void identityRevision;
  const computers = getAttachedBackends();
  const selection = telemetrySelection(kind);
  const preferred = readFrontendValue('selected-computer');
  const defaultComputer = computers.find((c) => c.home) ?? computers.find((c) => c.id === preferred) ?? computers[0];
  const seen = new Set<string>();
  return computers.flatMap((computer) => {
    const id = computer.backendId || (computer.home ? '@local' : computer.id);
    if (seen.has(id)) return [];
    seen.add(id);
    return [{
      id,
      key: computer.id,
      name: backendDisplayName(computer),
      selected: selection === null ? kind === 'usage' || computer === defaultComputer : selection.includes(id) || (computer.home && selection.includes('@local')),
      connected: backendReachable(computer.id),
    }];
  });
}

export function selectedTelemetryComputers(kind: TelemetryKind) { return telemetryComputers(kind).filter((computer) => computer.selected); }
export function missingTelemetrySelection(kind: TelemetryKind): number {
  const selected = telemetrySelection(kind);
  if (!selected) return 0;
  const computers = telemetryComputers(kind);
  const available = new Set(computers.map((computer) => computer.id));
  if (computers.some((computer) => computer.key === '')) available.add('@local');
  return selected.filter((id) => !available.has(id)).length;
}
export function toggleTelemetryComputer(kind: TelemetryKind, key: BackendKey): void {
  const selected = selectedTelemetryComputers(kind).map((c) => c.key);
  setTelemetrySelection(kind, selected.includes(key) ? selected.filter((id) => id !== key) : [...selected, key]);
}

export function telemetrySelectionLabel(kind: TelemetryKind): string {
  if (telemetrySelection(kind) === null && kind === 'usage') return 'All computers';
  const selected = selectedTelemetryComputers(kind);
  return selected.length === 1 ? selected[0].name : selected.length === 0 ? 'Choose computers' : `${selected.length} computers`;
}

export function resetTelemetryForTest(): void {
  usage = system = null;
  writeFrontendValue(keys.usage, null);
  writeFrontendValue(keys.system, null);
}
