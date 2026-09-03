// The one rune ./scopes.ts needs: whether the current read runs inside a
// reactive context (a `$derived`, an `$effect`, a template expression).
// A plain `.ts` module cannot call `$effect.tracking()`, and renaming
// scopes.ts would touch every import path in the app, so the question is
// answered here and imported.
export function inTrackingContext(): boolean {
  return $effect.tracking();
}
