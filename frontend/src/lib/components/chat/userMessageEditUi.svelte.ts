// Factory for `UserMessageEditSession.ui`.
//
// Its own module because the state has to be a rune — the editor row
// writes to it and the discard dialog renders from it — and runes only
// compile in `.svelte` / `.svelte.ts` files, while the session contract
// (`userMessageActions.ts`) is a plain type module every row imports.
//
// One factory rather than an object literal at each site so the session
// owner and the tests cannot disagree about what a fresh session starts
// with.

import type { UserMessageEditUiState } from './userMessageActions';

export function createUserMessageEditUiState(): UserMessageEditUiState {
  // focusPending starts true: creating this IS the reader opening the
  // editor. Every later mount is the virtualizer's and must not steal
  // focus — the row reads-and-clears the flag on mount.
  const ui = $state<UserMessageEditUiState>({
    focusPending: true,
    caret: null,
    confirmDiscard: false,
    commandError: '',
  });
  return ui;
}
