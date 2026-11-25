// Remove retired repository coordinates from older clients and caches.
export function safeIdentityError(value: string): string {
  const redact = (raw: string) => {
    // Apostrophes may belong to the password, as well as delimit Git's URL.
    const coordinate = raw.replace(/'+$/, '');
    return '[remote]' + raw.slice(coordinate.length);
  };
  return value.replace(/[a-z][a-z0-9+.-]*:\/\/[^\s<>"]+/gi, redact)
    .replace(/(?:[^\s<>"@/]+@)?[a-z0-9][a-z0-9._-]+:[^\s<>"]+/gi, redact);
}

const RETIRED_KEYS = ['remoteURL', 'remoteUrl', 'rootCommit', 'identitySource'] as const;
const NESTED_KEYS = ['project', 'thread', 'projects', 'threads', 'origin', 'rows'] as const;

/** Known metadata shapes only. Conversation items and arbitrary payloads are untouched. */
export function safeRepositoryMetadata<T>(value: T): T {
  if (Array.isArray(value)) {
    let next: unknown[] | undefined;
    for (let index = 0; index < value.length; index++) {
      const row = value[index];
      const safe = safeRepositoryMetadata(row);
      if (safe !== row) {
        if (!next) next = value.slice();
        next[index] = safe;
      }
    }
    return (next ?? value) as T;
  }
  if (!value || typeof value !== 'object') return value;
  const row = value as Record<string, unknown>;
  let next = row;
  for (let i = 0; i < RETIRED_KEYS.length; i++) {
    const key = RETIRED_KEYS[i];
    if (key in row) {
      if (next === row) next = { ...row };
      delete next[key];
    }
  }
  if (typeof row.identityError === 'string') {
    const safe = safeIdentityError(row.identityError);
    if (safe !== row.identityError) {
      if (next === row) next = { ...row };
      next.identityError = safe;
    }
  }
  if (!row.repositoryID && !row.identityError && (row.remoteURL || row.rootCommit)) {
    if (next === row) next = { ...row };
    next.identityError = 'Repository identity has not been verified yet.';
  }
  for (let i = 0; i < NESTED_KEYS.length; i++) {
    const key = NESTED_KEYS[i];
    const child = row[key];
    if (child && typeof child === 'object') {
      const safe = safeRepositoryMetadata(child);
      if (safe !== child) {
        if (next === row) next = { ...row };
        next[key] = safe;
      }
    }
  }
  return next as T;
}
