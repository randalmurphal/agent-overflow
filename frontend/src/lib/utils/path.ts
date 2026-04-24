// UI-only comparison for paths already supplied by the backend. This does not
// resolve symlinks; backend workspace mutations still perform canonical checks.
export function sameNormalizedPath(left: string | null | undefined, right: string | null | undefined): boolean {
  const normalizedLeft = normalizePath(left);
  const normalizedRight = normalizePath(right);
  return normalizedLeft !== '' && normalizedLeft === normalizedRight;
}

function normalizePath(path: string | null | undefined): string {
  const trimmed = (path ?? '').trim().replace(/\\/g, '/');
  if (trimmed === '') return '';
  const withoutTrailingSlash = trimmed.replace(/\/+$/, '');
  return withoutTrailingSlash || '/';
}
