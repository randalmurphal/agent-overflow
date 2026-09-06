// SSH configuration belongs to the frontend desktop that can execute it.
// Store host aliases/paths only; credentials stay in the user's SSH agent.
import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';
import { onBackendDetached } from '../transport/backends';

const KEY = 'computer-ssh';
export interface ComputerSSH { target: string; binary: string }
function readProfiles(): Record<string, ComputerSSH> {
  const raw = readFrontendValue(KEY);
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
  return Object.fromEntries(Object.entries(raw).slice(-64).filter(([id, value]) => id.length <= 256
    && value && typeof value === 'object' && !Array.isArray(value)
    && typeof value.target === 'string' && value.target.length > 0 && value.target.length <= 255
    && typeof value.binary === 'string' && value.binary.length > 0 && value.binary.length <= 4096));
}
let profiles = $state.raw<Record<string, ComputerSSH>>(readProfiles());
onFrontendValueChanged(KEY, () => { profiles = readProfiles(); });
export function computerSSH(id: string): ComputerSSH | undefined { return profiles[id]; }
export function saveComputerSSH(id: string, profile: ComputerSSH): void {
  const next = { ...profiles };
  delete next[id];
  next[id] = { target: profile.target, binary: profile.binary };
  profiles = Object.fromEntries(Object.entries(next).slice(-64));
  writeFrontendValue(KEY, profiles);
}
onBackendDetached(({ backendId }) => {
  const next = { ...profiles };
  if (!Object.hasOwn(next, backendId)) return;
  delete next[backendId];
  profiles = next;
  writeFrontendValue(KEY, next);
});
