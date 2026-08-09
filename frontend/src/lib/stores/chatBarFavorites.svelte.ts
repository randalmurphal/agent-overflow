// Chat-bar favorites — one app-global list.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): state is keyed by its
// ENTITY. Starred models and discussions are an APP fact: `ListChatBarFavorites`
// takes no arguments and `SetChatBarFavorite` returns the whole new list. Each
// mounted model menu used to hold its own copy behind a loaded-once latch, so
// starring in one pane left every other open menu showing the old list until it
// was closed and reopened — and the latch meant a failed first load never
// retried.
//
// One key, one loader, one retry curve; every menu derives from it.

import { ListChatBarFavorites, SetChatBarFavorite, type ChatBarFavorite } from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';

const KEY = 'app';

async function fetchFavorites(): Promise<ChatBarFavorite[]> {
  const res = (await ListChatBarFavorites()) as ChatBarFavorite[] | null;
  return Array.isArray(res) ? res : [];
}

const store = createEntityStore<ChatBarFavorite[], void>({
  name: 'chatBarFavorites',
  source: async ({ apply }) => {
    apply(await fetchFavorites());
    // Nothing to release: the list is a pull, not a subscription.
    return () => {};
  },
});

// The one hold. Retention is not refcounted here because the entity is the
// app: the list is cheap, every menu wants it, and dropping it when the last
// menu closes would only buy a re-fetch on the next open. Expressed as a
// permanent attachment rather than a retain-at-zero flag on the primitive:
// one hold that the test seam can release is the whole mechanism, and the
// primitive keeps exactly one retention rule.
let hold: EntityAttachment<ChatBarFavorite[]> | null = null;

/** Load the list once, lazily. Safe to call on every menu open. */
export function ensureChatBarFavorites(): void {
  hold ??= store.attach(KEY, undefined);
}

/** Reactive read; empty until the first load resolves. */
export function peekChatBarFavorites(): ChatBarFavorite[] {
  return store.peek(KEY) ?? [];
}

/** Reactive load error; null when healthy. */
export function peekChatBarFavoritesError(): string | null {
  return store.peekError(KEY);
}

/**
 * Star or unstar one favorite. The backend answers with the whole new list,
 * which lands on the shared entry — so every mounted menu updates, not just
 * the one that was clicked.
 */
export async function setChatBarFavorite(
  favorite: ChatBarFavorite,
  starred: boolean,
): Promise<void> {
  ensureChatBarFavorites();
  const updated = (await SetChatBarFavorite(favorite, starred)) as ChatBarFavorite[] | null;
  store.apply(KEY, Array.isArray(updated) ? updated : []);
}

/** Test seam: drop the entry and the hold, as a fresh module load would. */
export function __resetChatBarFavoritesForTest(): void {
  hold?.release();
  hold = null;
  store.suspend();
  store.resetAll();
}
