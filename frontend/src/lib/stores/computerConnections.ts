import type { BackendKey } from '../transport/backendKey';
import { backendById } from '../transport/backends';
import { isNativeShell } from '../native/platform';
import { repairPairedComputerAddress } from '../transport/deviceSession';
import { RepairBackendAddress } from './bindings';

/** Repair belongs to this frontend's connection, never the selected host's
 * settings. Capture the connection so removal/re-pair cannot redirect a retry. */
export async function repairComputerConnection(backend: BackendKey, address: string): Promise<string> {
  const connection = backendById(backend);
  if (!connection) throw new Error('This computer is no longer connected.');
  const value = address.trim();
  const endpoint = value.includes('://') ? value : `https://${value}`;
  const verified = isNativeShell() ? await repairPairedComputerAddress(backend, endpoint)
    : await RepairBackendAddress(backend, endpoint);
  if (backendById(backend) === connection) connection.client.triggerReconnect();
  return verified;
}
