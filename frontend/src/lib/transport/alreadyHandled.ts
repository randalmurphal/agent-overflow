import { TransportError } from './wsClient';

/**
 * Did this call lose a race to another client rather than fail?
 *
 * `already_handled` (internal/transport/frame.go) is the backend's answer when
 * the decision a call would have made was already made — an approval prompt
 * two screens both rendered and both answered, a form both submitted. One
 * backend now serves several clients, so this is ordinary, not exceptional.
 *
 * It is not an error to report. The caller wanted the question closed and the
 * question is closed; the only thing that did not happen is their answer being
 * the one that closed it. Surfacing it would put an error banner in front of
 * someone whose only mistake was answering a prompt that was still on their
 * screen. Nor is it retryable — a retry can only lose again.
 */
export function isAlreadyHandled(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'already_handled';
}

/**
 * Await a mutation that another client may have completed first, treating that
 * outcome as success. Every other failure rethrows unchanged.
 *
 * Use this at the call site of any RPC that answers a question the UI shows on
 * more than one screen, so the already-handled case cannot be forgotten in one
 * place and remembered in another.
 */
export async function ignoringAlreadyHandled(call: Promise<unknown>): Promise<void> {
  try {
    await call;
  } catch (err) {
    if (isAlreadyHandled(err)) return;
    throw err;
  }
}
