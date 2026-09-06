// DirectoryBrowser state module.
//
// Extracted from DirectoryBrowser.svelte to keep the .svelte shell focused
// on the listbox / path-input markup. Owns the in-flight browse state:
//
//   - `listing` — the currently-displayed DirectoryListing
//   - `pathText` — the string in the path input
//   - `highlight` — keyboard-nav index into `listing.entries`
//   - `loading` / `error` / `noMatches` — render flags
//   - debounce handle + browse-generation token so a slow response for an
//     older path can't overwrite the current listing
//
// Behaviour is unchanged vs. the inline version. The module returns the
// reactive state via getters plus the mutation helpers the UI binds to.

import { browseComputerDirectory } from '../../stores/computerProjects';
import { selectedBackend } from '../../stores/selectedBackend.svelte';
import type { BackendKey } from '../../transport/backendKey';
import type { DirectoryEntry, DirectoryListing } from '../../types/models';

export interface DirectoryBrowserOptions {
  /** Starting path to browse on mount. */
  initialPath: string;
  backend?: BackendKey;
  /** Fires every time the module commits to a committed (or blank) path. */
  onSelect?: (path: string) => void;
}

export interface DirectoryBrowserHandle {
  readonly listing: DirectoryListing | null;
  readonly pathText: string;
  readonly highlight: number;
  readonly loading: boolean;
  readonly error: string | null;
  readonly noMatches: boolean;

  setHighlight(next: number): void;

  /** Called once on mount — kicks off the initial browse. */
  mount(): void;
  /** Called on component destroy to clear any pending debounce timer. */
  destroy(): void;

  /** Browse explicit path — drill-in, goToParent, or initial load. */
  browse(path: string): Promise<void>;
  /** Debounced browse from typing in the path input. */
  handlePathInput(value: string): void;
  /** Immediate browse from Enter in the path input. */
  handlePathEnter(): void;
  /** Drill into an entry (no-op if it's a file). */
  drillInto(entry: DirectoryEntry): Promise<void>;
  /** Navigate to the listing's parent directory. */
  goToParent(): Promise<void>;
}

// Pull a readable message out of whatever the binding threw. Wails
// serializes server-side errors as objects with {message, kind, cause};
// naive String(err) prints "[object Object]". We want the message only.
function extractErrorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  if (err && typeof err === 'object') {
    const maybeMessage = (err as { message?: unknown }).message;
    if (typeof maybeMessage === 'string') return maybeMessage;
  }
  return 'Unknown error';
}

export function createDirectoryBrowser(opts: DirectoryBrowserOptions): DirectoryBrowserHandle {
  const backend = opts.backend ?? selectedBackend();
  let listing: DirectoryListing | null = $state(null);
  let highlight = $state(0);
  let pathText = $state(opts.initialPath);
  let loading = $state(false);
  let error: string | null = $state(null);
  let noMatches = $state(false);

  let debounceHandle: ReturnType<typeof setTimeout> | null = null;
  let browseToken = 0;

  async function browse(path: string, fromTyping = false): Promise<void> {
    const token = ++browseToken;
    loading = true;
    opts.onSelect?.('');
    if (!fromTyping) {
      // Only explicit nav (drill-in, goToParent, Enter, mount) clears a
      // prior error. Typing mustn't flicker the banner between keystrokes.
      error = null;
    }
    try {
      const result = (await browseComputerDirectory(backend, path)) as DirectoryListing;
      if (token !== browseToken) return;

      if (!result.exists) {
        // Server signals "nothing to list" for missing paths or files —
        // handle identically to a thrown error without the round-trip.
        await handleNonExistentBrowse(path, token, fromTyping);
        return;
      }

      listing = result;
      noMatches = false;
      error = null;
      // On explicit nav, adopt the server's canonical path so the input
      // mirrors the listing. While typing, leave pathText alone — the
      // server may normalise a trailing slash or expand "~", and
      // clobbering the input erases cursor position + keystrokes.
      if (!fromTyping) {
        pathText = result.path;
      }
      highlight = 0;
      opts.onSelect?.(result.path);
    } catch (err) {
      if (token !== browseToken) return;
      error = extractErrorMessage(err);
      listing = null;
      noMatches = false;
    } finally {
      if (token === browseToken) {
        loading = false;
      }
    }
  }

  async function handleNonExistentBrowse(
    path: string,
    token: number,
    fromTyping: boolean,
  ): Promise<void> {
    if (fromTyping) {
      const filtered = await tryPrefixFilter(path, token);
      if (token !== browseToken) return;
      if (filtered) {
        listing = filtered;
        noMatches = filtered.entries.length === 0;
      } else {
        listing = null;
        noMatches = true;
      }
      // Typed path isn't committable — tell the parent to disable Add
      // until the user drills into a real entry.
      opts.onSelect?.('');
      return;
    }
    error = `No such directory: ${path}`;
    listing = null;
    noMatches = false;
  }

  async function tryPrefixFilter(
    path: string,
    token: number,
  ): Promise<DirectoryListing | null> {
    const trimmed = path.replace(/[\\/]+$/, '');
    const lastSep = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'));
    if (lastSep < 0) return null;
    const separator = trimmed[lastSep];
    let parent = trimmed.slice(0, lastSep) || separator;
    if (/^[A-Za-z]:$/.test(parent)) parent += separator;
    const prefix = trimmed.slice(lastSep + 1);
    if (!prefix) return null;
    const parentListing = (await browseComputerDirectory(backend, parent)) as DirectoryListing;
    if (token !== browseToken || !parentListing.exists) return null;
    const prefixLower = prefix.toLowerCase();
    return {
      ...parentListing,
      entries: parentListing.entries.filter((e) => e.name.toLowerCase().startsWith(prefixLower)),
    };
  }

  async function drillInto(entry: DirectoryEntry): Promise<void> {
    if (!entry.isDir || !listing) return;
    const sep = listing.separator || '/';
    const next = listing.path.endsWith(sep)
      ? listing.path + entry.name
      : `${listing.path}${sep}${entry.name}`;
    await browse(next);
  }

  async function goToParent(): Promise<void> {
    if (!listing || !listing.parent) return;
    await browse(listing.parent);
  }

  return {
    get listing() { return listing; },
    get pathText() { return pathText; },
    get highlight() { return highlight; },
    get loading() { return loading; },
    get error() { return error; },
    get noMatches() { return noMatches; },

    setHighlight(next: number): void {
      highlight = next;
    },

    mount(): void {
      void browse(opts.initialPath);
    },

    destroy(): void {
      ++browseToken;
      if (debounceHandle) clearTimeout(debounceHandle);
    },

    async browse(path: string): Promise<void> {
      await browse(path);
    },

    handlePathInput(value: string): void {
      pathText = value;
      ++browseToken;
      opts.onSelect?.('');
      if (debounceHandle) clearTimeout(debounceHandle);
      debounceHandle = setTimeout(() => {
        debounceHandle = null;
        void browse(pathText, true);
      }, 120);
    },

    handlePathEnter(): void {
      if (debounceHandle) {
        clearTimeout(debounceHandle);
        debounceHandle = null;
      }
      void browse(pathText);
    },

    drillInto,
    goToParent,
  };
}
