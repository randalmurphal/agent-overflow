// Stars are frontend preferences. A legacy host list seeds this device once;
// subsequent edits work offline and never change another frontend's choices.
import { untrack } from 'svelte';
import { isProviderID } from '../types/providers';
import { ListChatBarFavorites, type ChatBarFavorite } from './bindings';
import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';
import { selectedBackend } from './selectedBackend.svelte';
import { backendById, withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import { userFacingError } from '../utils/userFacingError';

const KEY = 'chat-bar-favorites';
const LIMIT = 256;
function identity(row: ChatBarFavorite): string { return `${row.kind}:${row.provider ?? ''}:${row.value}`; }
function validated(value: unknown): ChatBarFavorite[] {
  if (!Array.isArray(value)) return [];
  const rows = new Map<string, ChatBarFavorite>();
  for (const raw of value.slice(0, LIMIT)) {
    if (!raw || typeof raw !== 'object' || (raw.kind !== 'model' && raw.kind !== 'discussion')
      || typeof raw.value !== 'string' || !raw.value || raw.value.length > 4096
      || typeof raw.label !== 'string' || raw.label.length > 1024
      || (raw.kind === 'model' && !isProviderID(raw.provider))) continue;
    const row: ChatBarFavorite = { kind: raw.kind, value: raw.value, label: raw.label,
      createdAt: Number.isSafeInteger(raw.createdAt) && raw.createdAt >= 0 ? raw.createdAt : 0 };
    if (row.kind === 'model') row.provider = raw.provider;
    rows.set(identity(row), row);
  }
  return [...rows.values()];
}
function read(): ChatBarFavorite[] | null {
  const saved = readFrontendValue(KEY);
  return saved === null ? null : validated(saved);
}
let favorites = $state<ChatBarFavorite[] | null>(read());
let error = $state<string | null>(null);
let loading = false;
let revision = 0;

/** Lazy, one-time migration. A failed seed can retry on the next menu open. */
export function ensureChatBarFavorites(): void {
  untrack(() => {
    if (favorites !== null || loading) return;
    const backend = selectedBackend();
    const entry = backendById(backend);
    if (!entry || !hasScope('settings:read', backend)) return;
    const version = revision;
    loading = true;
    error = null;
    void withBackendTarget(backend, () => ListChatBarFavorites()).then((rows) => {
      if (version !== revision || backendById(backend) !== entry) return;
      // Another window may have saved stars while this migration was in flight.
      favorites = read() ?? validated(rows);
      writeFrontendValue(KEY, favorites);
    }).catch((reason) => {
      if (version === revision && backendById(backend) === entry) error = userFacingError(reason);
    }).finally(() => { if (version === revision) loading = false; });
  });
}

export function peekChatBarFavorites(): ChatBarFavorite[] { return favorites ?? []; }
export function peekChatBarFavoritesError(): string | null { return error; }

export async function setChatBarFavorite(favorite: ChatBarFavorite, starred: boolean): Promise<void> {
  const row = validated([favorite])[0];
  if (!row) throw new Error('Invalid favorite.');
  const key = identity(row);
  const previous = read() ?? favorites ?? [];
  const updated = previous.filter((entry) => identity(entry) !== key);
  if (starred) {
    if (updated.length >= LIMIT) throw new Error('Remove a favorite before adding another.');
    updated.unshift(row);
  }
  if (!writeFrontendValue(KEY, updated)) throw new Error('This device could not save its favorites.');
  ++revision;
  loading = false;
  error = null;
  favorites = updated;
}

onFrontendValueChanged(KEY, () => {
  ++revision;
  loading = false;
  error = null;
  favorites = read();
});

export function __resetChatBarFavoritesForTest(): void {
  ++revision;
  loading = false;
  error = null;
  favorites = read();
}
