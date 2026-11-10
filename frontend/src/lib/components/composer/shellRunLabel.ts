/** The last segment of a workspace path, for naming where a command runs. */
export function workspaceLabel(path: string | null | undefined): string {
  const trimmed = (path ?? '').replace(/[\\/]+$/, '');
  if (!trimmed) return 'the workspace';
  const cut = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'));
  return cut >= 0 ? trimmed.slice(cut + 1) : trimmed;
}
