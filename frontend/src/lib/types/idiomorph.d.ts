// Shim type declaration for the `idiomorph` package, which ships
// without bundled .d.ts files. The library's only public surface is
// the `Idiomorph.morph(...)` entry point; we type that just enough
// for the AnsiText callsite. Upstream may eventually publish types,
// at which point this can be deleted.

declare module 'idiomorph' {
  /** Options that drive how Idiomorph reconciles two trees. */
  export interface IdiomorphOptions {
    /**
     * `'innerHTML'` morphs only the children of `oldNode` against
     * `newNode`'s children, leaving `oldNode` itself untouched.
     * `'outerHTML'` (default) replaces `oldNode` with a morph of
     * `newNode`. We use innerHTML mode in AnsiText so the `<pre>`
     * shell stays put across updates.
     */
    morphStyle?: 'outerHTML' | 'innerHTML';
    /** When true, the active element is left alone during the morph. */
    ignoreActive?: boolean;
    /** When true, the active element's value attribute is preserved. */
    ignoreActiveValue?: boolean;
    /** When true (default), focus + selection are restored after the morph. */
    restoreFocus?: boolean;
    /** Optional callbacks that fire at points in the morph lifecycle. */
    callbacks?: Record<string, (...args: unknown[]) => unknown>;
  }

  /**
   * Morph an existing DOM tree toward new content, applying minimal
   * patches. See https://github.com/bigskysoftware/idiomorph.
   *
   * `newContent` mirrors the union idiomorph's own `normalizeParent`
   * accepts at runtime: an HTML string, a single `Node`, or an array
   * of `Node`s (e.g. a detached element's `childNodes`). AnsiText hands
   * over child nodes rather than a wrapper element so the morph diffs
   * them straight into root's children instead of nesting the wrapper.
   */
  export const Idiomorph: {
    morph(
      oldNode: Element,
      newContent: string | Node | Node[],
      options?: IdiomorphOptions,
    ): void;
  };
}
