/**
 * Emoji shortcodes on a forge surface (forgeEmojiExtension.ts), loaded on
 * demand: the grammar and its name table stay out of the entry chunk.
 * Until they arrive, a forge surface's extension list holds the forge's
 * slot, which claims nothing; `resolveForgeEmoji` then returns the loaded
 * extension, whose `tokenizesAlike` keeps every block that holds no known
 * shortcode.
 */
import type { Extension } from '../markdown';
import type { EmojiForge } from './forgeEmojiExtension';
import { reportFrontendDiagnostic } from './frontendErrorCapture';

type ExtensionModule = Pick<typeof import('./forgeEmojiExtension'), 'buildForgeEmojiExtensions'>;

// Each forge's place in an extension list while its table loads: claims nothing.
const PENDING: Record<EmojiForge, Extension> = {
  github: { name: 'forgeEmoji', level: 'inline', tokenizer: () => undefined },
  gitlab: { name: 'forgeEmoji', level: 'inline', tokenizer: () => undefined },
};

let importExtension: () => Promise<ExtensionModule> = () => import('./forgeEmojiExtension');
let loaded = $state.raw<Record<EmojiForge, Extension> | null>(null);
let request: Promise<void> | null = null;

/** Loads the extensions once; the promise is stable and never rejects. A failed
 * load is reported and leaves shortcodes as text. */
export function loadForgeEmoji(): Promise<void> {
  request ??= importExtension().then(
    ({ buildForgeEmojiExtensions }) => {
      loaded = buildForgeEmojiExtensions(PENDING);
    },
    (err: unknown) => {
      reportFrontendDiagnostic('Forge emoji table failed to load', err instanceof Error ? err.message : String(err));
    },
  );
  return request;
}

/** The emoji slot for a `forge` surface's extension list; starts the load. */
export function forgeEmojiSlot(forge: EmojiForge): Extension {
  void loadForgeEmoji();
  return PENDING[forge];
}

/** `extension`, or the loaded emoji extension when it is a slot whose table
 * has arrived. A reactive read for a slot only. */
export function resolveForgeEmoji(extension: Extension): Extension {
  if (extension === PENDING.github) return loaded?.github ?? extension;
  if (extension === PENDING.gitlab) return loaded?.gitlab ?? extension;
  return extension;
}

/** Changes once, when the table arrives. */
export function forgeEmojiGeneration(): number {
  return loaded === null ? 0 : 1;
}

/** Back to before the load, with `load` as the extension import. */
export function __resetForgeEmojiForTest(load?: () => Promise<ExtensionModule>): void {
  importExtension = load ?? (() => import('./forgeEmojiExtension'));
  loaded = null;
  request = null;
}
