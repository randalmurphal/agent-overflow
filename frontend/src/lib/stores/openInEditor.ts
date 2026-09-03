// Single frontend boundary for the host-scoped OpenInEditor RPC. Components
// also hide or disable their affordances when the page cannot act on the host
// desktop, but keeping this check next to the binding prevents a missed UI
// guard from reaching the wire.

import { hasScope } from '../transport/scopes';
import { OpenInEditor } from './bindings';

export async function openInEditor(
  path: string,
  line: number,
  col: number,
  workspacePath: string,
  editorID: string,
): Promise<void> {
  if (!hasScope('host')) {
    throw new Error('Opening files in an editor needs the app running on this computer.');
  }
  await OpenInEditor(path, line, col, workspacePath, editorID);
}
