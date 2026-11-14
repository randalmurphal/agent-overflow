import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ParseCommentSuggestions } from './bindings';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { Block } from '../../../bindings/agent-overflow/internal/mdsuggest/models';

// The Preview toggle of every review editor (a new comment, a reply, an
// edit of a draft or a sent comment, the PR comment, the review summary,
// the description), kept with the editors' text in the review state: an
// editor is a virtualized row and unmounts. An editor in preview records
// where its caret was and how tall its text box stood, so turning
// preview off puts the caret back and the preview holds the box's
// height (the editor does not jump). An editor that closes loses its
// preview: the host names which editors are open. Where a previewed body's
// suggestion fences are is the backend's answer (each control's `parse`, on the
// pane's computer).

/** An editor in preview: its caret's selection and the text box's height. */
export interface EditorPreviewState {
  readonly start: number;
  readonly end: number;
  readonly height: number;
}

/** The suggestion blocks of a body, as the backend reads them. */
export type SuggestionParse = (body: string) => Promise<readonly Block[]>;

/** One editor's preview, for its toggle and shell. */
export interface EditorPreviewControl {
  /** Null while the editor shows its text. */
  readonly state: EditorPreviewState | null;
  open(state: EditorPreviewState): void;
  /** Leaves preview; answers where the caret was. */
  close(): EditorPreviewState | null;
  /** Reads a previewed body's suggestion blocks. */
  readonly parse: SuggestionParse;
}

export interface EditorPreviews {
  control(key: string): EditorPreviewControl;
  dispose(): void;
}

/** The keys of the editors that have no comment write key. */
export const EDITOR_PREVIEW_KEY = {
  summary: 'summary',
  description: 'description',
  draftEdit: (draftId: string) => `draft-edit:${draftId}`,
} as const;

export function createEditorPreviews(openKeys: () => ReadonlySet<string>, backend: BackendKey): EditorPreviews {
  const states = new SvelteMap<string, EditorPreviewState>();
  const parseSuggestions: SuggestionParse = async (body) => (await withBackendTarget(backend, () => ParseCommentSuggestions(body))) ?? [];
  const stopPrune = $effect.root(() => {
    $effect(() => {
      const open = openKeys();
      untrack(() => {
        for (const key of states.keys()) if (!open.has(key)) states.delete(key);
      });
    });
  });
  return {
    control(key) {
      return {
        get state() { return states.get(key) ?? null; },
        open(state) { states.set(key, state); },
        close() {
          const was = states.get(key) ?? null;
          states.delete(key);
          return was;
        },
        parse: parseSuggestions,
      };
    },
    dispose: stopPrune,
  };
}
