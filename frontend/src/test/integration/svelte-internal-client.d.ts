// Minimal typing for svelte's private runtime entry. svelte does not ship
// declarations for 'svelte/internal/client'; only the svelte-patch
// regression suites (svelte-patch-zombie-leak.test.ts,
// svelte-patch-ownerless-roots.test.ts) import it. Add members only as
// those tests need them, and never import this module from production
// code.
declare module 'svelte/internal/client' {
  export interface ValueLike<V = unknown> {
    v: V;
    reactions: object[] | null;
  }
  export interface EffectLike {
    ctx: object | null;
    parent: EffectLike | null;
  }
  export function state<V>(value: V): ValueLike<V>;
  export function get<V>(signal: ValueLike<V>): V;
  export function set<V>(signal: ValueLike<V>, value: V): V;
  /** Live binding: reads reflect the currently-running effect. */
  export const active_effect: EffectLike | null;
}
