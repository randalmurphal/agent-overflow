export function pathBasename(path: string | undefined): string {
  if (!path) return '';
  const trimmed = path.trim().replace(/[\\/]+$/, '');
  if (!trimmed) return '';
  const slash = trimmed.lastIndexOf('/');
  const backslash = trimmed.lastIndexOf('\\');
  const idx = Math.max(slash, backslash);
  return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
}
