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

/** Known metadata shapes only. Conversation items and arbitrary payloads are untouched. */
export function safeRepositoryMetadata<T>(value: T): T {
  if (Array.isArray(value)) {
    let next: unknown[] | undefined;
    value.forEach((row, index) => {
      const safe = safeRepositoryMetadata(row);
      if (safe !== row) { next ??= value.slice(); next[index] = safe; }
    });
    return (next ?? value) as T;
  }
  if (!value || typeof value !== 'object') return value;
  const row = value as Record<string, unknown>;
  let next = row;
  const set = (key: string, safe: unknown) => {
    if (safe === row[key]) return;
    if (next === row) next = { ...row };
    next[key] = safe;
  };
  for (const key of ['remoteURL', 'remoteUrl', 'rootCommit', 'identitySource']) {
    if (key in row) {
      if (next === row) next = { ...row };
      delete next[key];
    }
  }
  if (typeof row.identityError === 'string') set('identityError', safeIdentityError(row.identityError));
  if (!row.repositoryID && !row.identityError && (row.remoteURL || row.rootCommit)) {
    set('identityError', 'Repository identity has not been verified yet.');
  }
  for (const key of ['project', 'thread', 'projects', 'threads', 'origin', 'rows']) {
    if (row[key] && typeof row[key] === 'object') set(key, safeRepositoryMetadata(row[key]));
  }
  return next as T;
}
