import { afterEach, describe, expect, it } from 'vitest';
import {
  confirmUnsentMessageRestore,
  hasPendingUnsentMessageConfirmation,
  resetUnsentMessageConfirmationForTest,
  resolveUnsentMessageConfirmation,
} from './unsentMessageConfirmation.svelte';

describe('unsent-message confirmation', () => {
  afterEach(() => {
    resetUnsentMessageConfirmationForTest();
  });

  it('is closed until somebody asks', () => {
    expect(hasPendingUnsentMessageConfirmation()).toBe(false);
    // A stray answer with nothing open is a no-op, not a throw: the host
    // renders from the same flag, so this only happens on a torn-down page.
    resolveUnsentMessageConfirmation(true);
    expect(hasPendingUnsentMessageConfirmation()).toBe(false);
  });

  it('settles the asker with the answer, and closes', async () => {
    const putBack = confirmUnsentMessageRestore();
    expect(hasPendingUnsentMessageConfirmation()).toBe(true);
    resolveUnsentMessageConfirmation(true);
    expect(await putBack).toBe(true);
    expect(hasPendingUnsentMessageConfirmation()).toBe(false);

    const leaveIt = confirmUnsentMessageRestore();
    resolveUnsentMessageConfirmation(false);
    expect(await leaveIt).toBe(false);
  });

  it('settles an older question as "leave it" when a second one arrives', async () => {
    const first = confirmUnsentMessageRestore();
    const second = confirmUnsentMessageRestore();

    // Two dialogs cannot both be on screen, and the older question is about
    // the older message — answering it by dismissal would put text back
    // BEHIND the text the newer answer is about.
    expect(await first).toBe(false);
    expect(hasPendingUnsentMessageConfirmation()).toBe(true);

    resolveUnsentMessageConfirmation(true);
    expect(await second).toBe(true);
  });

  it('settles a pending question when the page is torn down', async () => {
    const pending = confirmUnsentMessageRestore();
    resetUnsentMessageConfirmationForTest();
    // Never left hanging: an awaiting send would otherwise hold its snapshot
    // and its catch branch open forever.
    expect(await pending).toBe(false);
    expect(hasPendingUnsentMessageConfirmation()).toBe(false);
  });
});
