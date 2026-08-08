// Which key press means what on the import surface.
//
// Split out of SessionImportModal because it is a decision, not a rendering
// concern: every branch is about who owns a key — the surface, or the
// focused control's own native handling — and that question is answerable
// from the event alone. Plain TS (threadRowActions.ts style) so it is
// testable without mounting a modal.
//
// Contract: a returned action ALWAYS means the surface takes the key, so the
// caller preventDefault()s exactly when this returns non-null. Returning
// null means "leave it to the platform" — never "do nothing".

export type ImportKeyAction =
  | 'select-all'
  | 'cursor-down'
  | 'cursor-up'
  | 'toggle-active'
  | 'run-import';

/** The parts of a KeyboardEvent this decision reads. */
export interface ImportKeyEvent {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  target: EventTarget | null;
}

export function resolveImportKeyAction(event: ImportKeyEvent): ImportKeyAction | null {
  const target = event.target as HTMLElement | null;
  const tag = target?.tagName ?? '';
  // Controls whose own key handling must win: the project <select>'s
  // arrows, and Space/Enter on the select-all checkbox and the segments.
  const nativeKeyControl = tag === 'SELECT' || tag === 'BUTTON' || tag === 'INPUT';
  const textField = tag === 'INPUT' && (target as HTMLInputElement).type === 'search';

  if ((event.metaKey || event.ctrlKey) && (event.key === 'a' || event.key === 'A')) {
    // In a non-empty search box mod+a is text select-all and stays that way.
    // On an empty box the native meaning is a no-op, so the bulk-selection
    // meaning takes it — which is exactly the moment the modal opens with
    // search focused and nothing typed.
    if (textField && (target as HTMLInputElement).value !== '') return null;
    return 'select-all';
  }

  switch (event.key) {
    case 'ArrowDown':
      return tag === 'SELECT' ? null : 'cursor-down';
    case 'ArrowUp':
      return tag === 'SELECT' ? null : 'cursor-up';
    case ' ':
      return nativeKeyControl ? null : 'toggle-active';
    case 'Enter':
      return tag === 'SELECT' || tag === 'BUTTON' ? null : 'run-import';
    default:
      return null;
  }
}
