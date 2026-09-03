/**
 * The "sessions already running keep this until they restart" notice, shared
 * by the two Settings editors whose saves can defer a restart: the thinking
 * axis in ClaudeSessionAxesEditor and the peer inbox in
 * ClaudeCrossSessionEditor.
 *
 * The notice is a claim about a SAVE, not about a value, so it cannot be a
 * plain `$derived` over settings: the same stored value can be the result of
 * a change that deferred a restart or of one that never happened. But a plain
 * latch is wrong in the other direction, which is what this replaces. It
 * survived the two events that make it false, and the frontend can see both:
 *
 *   - the save FAILED. `updateSettingsPatch` never rejects — it restores the
 *     keys it patched and toasts — so a caller that latched on the way in has
 *     no idea, and the user is left reading a restart warning for a setting
 *     that was never stored.
 *   - the axis changed AGAIN, here or in another window (the settings event
 *     lands in the same store). The old notice then describes a value nobody
 *     is running toward any more.
 *
 * So: a latch plus a witness. Armed with the stored signature of the value
 * that was just saved, and rendered only while the stored signature still
 * matches it. Both events above move the signature, and one rule covers them.
 *
 * What no signature can observe is the sessions actually restarting, which is
 * the event that finally makes the notice untrue. Nothing on the wire reports
 * a thread's pending config restart today — `pendingConfigReconnects` lives
 * entirely inside app_session_config.go — so deriving THAT half means new
 * App-bound surface (and an `//ao:scope` annotation for it). This is
 * the half that needs none.
 *
 * Usage (from a `.svelte` component, since it uses runes):
 *
 *   const notice = createDeferredRestartNotice(() => `${enabled}:${inbound}`);
 *   // in the save handler, AFTER updateSettingsPatch has written optimistically:
 *   if (changed) notice.arm();
 */
export interface DeferredRestartNotice {
  /**
   * Record that the value now in the store is waiting on a restart. Call it
   * after the optimistic write, so the captured signature is the saved one.
   */
  arm(): void;
  /** Forget the notice outright. */
  clear(): void;
  /** True while the armed value is still the stored one. */
  readonly visible: boolean;
}

export function createDeferredRestartNotice(
  storedSignature: () => string,
): DeferredRestartNotice {
  let armed = $state<string | null>(null);
  return {
    arm(): void {
      armed = storedSignature();
    },
    clear(): void {
      armed = null;
    },
    get visible(): boolean {
      return armed !== null && armed === storedSignature();
    },
  };
}
