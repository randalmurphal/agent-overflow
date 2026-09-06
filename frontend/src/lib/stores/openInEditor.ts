// Single frontend boundary for the host-scoped OpenInEditor RPC. Components
// also hide or disable their affordances when the page cannot act on the host
// desktop, but keeping this check next to the binding prevents a missed UI
// guard from reaching the wire.

import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import { OpenInEditor } from './bindings';

export async function openInEditor(
  backend: BackendKey,
  path: string,
  line: number,
  col: number,
  workspacePath: string,
  editorID: string,
): Promise<void> {
  if (!hasScope('host', backend)) {
    throw new Error('Opening files in an editor needs the app running on this computer.');
  }
  await withBackendTarget(backend, () => OpenInEditor(path, line, col, workspacePath, editorID));
}
