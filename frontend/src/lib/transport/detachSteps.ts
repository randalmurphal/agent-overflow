// What has to happen on a backend BEFORE this client stops talking to it.
//
// A leaf on purpose. There are exactly two doors out of a connection —
// `backendAttach.detachAttachedBackend` for a machine this client
// attached, and `deviceSession.unpairHome` for its own — and both live in
// this directory, while the work they now have to do (withdrawing this
// phone's push registration) lives in `native/`. A direct import either
// way would either put a Capacitor seam inside the transport or close a
// cycle between two modules that already import each other, so the shell
// INSTALLS its step here and the two doors call it.
//
// Same shape and same reason as `backends.setBackendSource`: one
// function, replaced rather than branched on, so nothing below it has to
// know which kind of client it is running in.
//
// **Steps are fire-and-forget and must not throw.** A backend that is
// unreachable at the moment it is detached cannot be told anything, and
// failing the removal over it would leave a person with a machine they
// cannot get rid of. The step is issued BEFORE the socket closes, which
// is the only moment its RPC can still go out.

import type { BackendKey } from './backendKey';

const steps = new Set<(backend: BackendKey) => void>();

/**
 * Register something to run against a backend just before this client
 * lets go of it. Answers a remover.
 */
export function onBeforeBackendDetach(step: (backend: BackendKey) => void): () => void {
  steps.add(step);
  return () => {
    steps.delete(step);
  };
}

/** Run every registered step. Called by the two removal doors only. */
export function runBeforeBackendDetach(backend: BackendKey): void {
  for (const step of steps) {
    try {
      step(backend);
    } catch (err) {
      console.warn('transport: a detach step threw', err);
    }
  }
}

/** Test seam: forget every installed step. */
export function __resetDetachStepsForTest(): void {
  steps.clear();
}
