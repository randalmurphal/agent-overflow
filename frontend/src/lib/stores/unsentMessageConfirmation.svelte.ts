/**
 * The one question a send that lost its socket has to ask.
 *
 * A send whose connection died AFTER the retry has also failed is genuinely
 * undecidable on this side: the frame may have reached the agent, or it may
 * not. Putting the text back silently is a guess, and the wrong guess sends
 * the same message twice. So the composer asks, once, and the answer is the
 * person's.
 *
 * A promise-shaped door rather than a component prop because the asker is
 * `composerSend.ts` — a plain async function with no place to render. The
 * host (`components/composer/UnsentMessageConfirmationHost.svelte`) mounts
 * once at the app root and settles whatever is pending.
 */

interface PendingUnsentMessage {
  resolve: (restore: boolean) => void;
}

let pending: PendingUnsentMessage | null = $state(null);

/** True while the question is on screen. */
export function hasPendingUnsentMessageConfirmation(): boolean {
  return pending !== null;
}

/**
 * Ask whether to put an undelivered message back in the composer. Resolves
 * true for "Put it back", false for "Leave it".
 *
 * A second ask while one is open settles the FIRST as "leave it" and takes
 * its place: two dialogs cannot both be on screen, and the older question is
 * about the older message — answering it by dismissal would put text back
 * behind the text the newer answer is about.
 */
export function confirmUnsentMessageRestore(): Promise<boolean> {
  const previous = pending;
  const promise = new Promise<boolean>((resolve) => {
    pending = { resolve };
  });
  previous?.resolve(false);
  return promise;
}

/** Settle the open question. No-op when nothing is pending. */
export function resolveUnsentMessageConfirmation(restore: boolean): void {
  const current = pending;
  pending = null;
  current?.resolve(restore);
}

export function resetUnsentMessageConfirmationForTest(): void {
  const current = pending;
  pending = null;
  current?.resolve(false);
}
