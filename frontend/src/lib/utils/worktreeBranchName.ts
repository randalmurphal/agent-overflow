// Generate a short, prefixed worktree branch name client-side so the
// composer's WorktreeNameInput can prefill what the user is going to get.
// Mirrors the backend's BuildTemporaryWorktreeBranchNameWithPrefix shape
// (`<prefix><8 hex>`) — the backend regenerates if the input is empty,
// so the prefill is purely a visibility aid.

const DEFAULT_PREFIX = 'ao-';

export function generateWorktreeBranchName(prefix: string = DEFAULT_PREFIX): string {
  const cleaned = (prefix || DEFAULT_PREFIX).trim().toLowerCase() || DEFAULT_PREFIX;
  const bytes = new Uint8Array(4);
  crypto.getRandomValues(bytes);
  let hex = '';
  for (const b of bytes) {
    hex += b.toString(16).padStart(2, '0');
  }
  return `${cleaned}${hex}`;
}
