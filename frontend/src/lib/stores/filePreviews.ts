import type { BackendKey } from '../transport/backendKey';
import { backendById, withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import { handleExternalURL } from '../utils/externalLinks';
import { MintFilePreviewURL } from './bindings';
import { getTransportHelloFor } from './transportStatus.svelte';

export function canPreviewFiles(backend: BackendKey): boolean {
  return hasScope('preview:open', backend)
    && (getTransportHelloFor(backend)?.capabilities.includes('preview.files.v1') ?? false);
}

export async function openFilePreview(backend: BackendKey, path: string, workspace: string): Promise<void> {
  if (!canPreviewFiles(backend)) throw new Error('Update this computer to open HTML previews.');
  const owner = backendById(backend);
  if (!owner || owner.status.status !== 'connected') throw new Error('Reconnect to this computer to open the preview.');
  const url = await withBackendTarget(backend, () => MintFilePreviewURL(path, workspace));
  // Forget/re-pair during the RPC must not open a stale computer's response.
  if (backendById(backend) !== owner) throw new Error('This computer connection changed. Open the preview again.');
  if (url) await handleExternalURL(url);
}
