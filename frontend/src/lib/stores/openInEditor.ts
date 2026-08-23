// Single frontend boundary for the LocalOnly OpenInEditor RPC. Components also
// hide or disable their affordances in view-only sessions, but keeping this
// check next to the binding prevents a missed UI guard from reaching the wire.

import { isViewOnlySession } from '../transport/runMode';
import { OpenInEditor } from './bindings';

export async function openInEditor(
  path: string,
  line: number,
  col: number,
  workspacePath: string,
  editorID: string,
): Promise<void> {
  if (isViewOnlySession()) {
    throw new Error('Opening files in an editor is unavailable in a view-only session.');
  }
  await OpenInEditor(path, line, col, workspacePath, editorID);
}
