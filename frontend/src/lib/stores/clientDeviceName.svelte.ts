import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';
import { suggestDeviceLabel } from '../utils/deviceLabel';

const KEY = 'device-name';
function read(): string | null {
  const value = readFrontendValue(KEY);
  return typeof value === 'string' && Array.from(value).length <= 80 && !/\p{Cc}/u.test(value)
    ? value.trim() : null;
}
let saved = $state(read());
let status = $state('');
const listeners = new Set<() => void>();

export function clientDeviceName(): string { return saved || suggestDeviceLabel(); }
export function hasClientDeviceName(): boolean { return saved !== null; }
export function clientDeviceNameStatus(): string { return status; }
export function setClientDeviceNameStatus(value: string): void { status = value; }

export function saveClientDeviceName(value: string): void {
  const name = value.trim();
  if (Array.from(name).length > 80 || /\p{Cc}/u.test(name)) {
    throw new Error('Use a device name of 80 characters or fewer without control characters.');
  }
  if (!writeFrontendValue(KEY, name)) throw new Error('Could not save this device’s name.');
  saved = name;
  for (const listener of listeners) listener();
}

export function onClientDeviceNameChanged(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

onFrontendValueChanged(KEY, () => {
  saved = read();
  for (const listener of listeners) listener();
});

export function resetClientDeviceNameForTest(): void { saved = read(); }
