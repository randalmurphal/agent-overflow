// What this client throws away when a credential stops being usable.
//
// A leaf, and a seam, for the same reason `./detachSteps.ts` is one: the
// things that have to go (the IndexedDB thread replica, the `appStorage`
// bucket) live above the transport, and the transport is where the three
// moments are. A direct import would point `transport/` at `replica/` and
// `stores/`, which is the wrong direction and, for the replica, a ring
// (`replica/session.ts` reads `transport/backendIdentity.ts`). So the
// owners REGISTER, and the transport states the moment.
//
// The three moments, and they are the whole list:
//
//   - **Sign-out.** `deviceSession.unpairHome()`, the phone shell's "Pair
//     again" and the only door a fixed-origin page has. Every backend
//     goes, because the page is about to become a first run.
//   - **Detach.** `backendAttach.detachAttachedBackend()` and Settings →
//     Systems. One backend goes.
//   - **A refused credential.** `deviceSession`'s 401 branch, where the
//     session family has ended and nothing about waiting brings it back.
//     One backend goes.
//
// Spec §9 names `purgeReplicaDatabases(liveBackendIds)` as the primitive
// "sign-out and revocation will call" and lists the CALLERS as open. This
// module is those callers, expressed once.
//
// **Steps are fire-and-forget and must not throw.** A purge runs on a path
// that is already taking something away, and failing it would leave a
// person with a machine they cannot get rid of. A step that throws is
// contained and logged; the rest still run. They may return a promise and
// nothing awaits it: `unpairHome` reloads the page immediately afterwards,
// and a deletion that outlives the document is the storage engine's own
// business, which is why the sign-out step deletes rather than rewrites.

import { type BackendKey } from './backendKey';

/**
 * `null` means EVERY backend, which is a sign-out. It is deliberately not
 * the empty string, which is the home backend's own registry id: "drop
 * home" and "drop everything" are different instructions, and spelling
 * them the same is how one becomes the other by accident.
 */
export type PurgeScope = BackendKey | null;

export type ClientPurgeStep = (scope: PurgeScope) => void | Promise<void>;

const steps = new Set<ClientPurgeStep>();

/**
 * Register something this client holds on a backend's behalf. Answers a
 * remover. Called at module load by the owners (`replica/session.ts`,
 * `stores/appStorage.ts`), so importing them is the whole wiring.
 */
export function onPurgeClientState(step: ClientPurgeStep): () => void {
  steps.add(step);
  return () => {
    steps.delete(step);
  };
}

/** Drop everything this client holds for `scope`. Never throws. */
export function purgeClientState(scope: PurgeScope): void {
  for (const step of steps) {
    try {
      const settled = step(scope);
      if (settled instanceof Promise) {
        settled.catch((err: unknown) => {
          console.warn('transport: a client-purge step failed', err);
        });
      }
    } catch (err) {
      console.warn('transport: a client-purge step threw', err);
    }
  }
}

/** Test seam: forget every registered step. */
export function __resetClientPurgeForTest(): void {
  steps.clear();
}
