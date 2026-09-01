// The banner's other job, and the one that is not a sentence: a page that
// mounted LATCHED loaded nothing, so the first connection it ever gets has
// to be followed by a boot.
//
// Its own file, and a `.svelte.ts` one, because these cases need the
// snapshot to CHANGE while the component is mounted. The sibling suite
// drives a plain holder object and picks its state before rendering, which
// is all its assertions need; a transition needs the mocked accessor to be
// reactive, and `$state` is only available in a Svelte-compiled module.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { TransportStatusSnapshot } from '../../transport/wsClient';

let snapshot = $state<TransportStatusSnapshot>({ status: 'connected', nextAttemptAt: null });

// Partial mock, for the reason the sibling suite states: a whole-module
// factory would turn every export this file does not name into undefined.
// The accessor closes over `snapshot` rather than capturing it, so the
// factory running during the component's import (before this module's own
// initialisers) reads nothing.
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => snapshot,
}));

import TransportStatusBanner from './TransportStatusBanner.svelte';

async function publish(status: TransportStatusSnapshot['status']): Promise<void> {
  snapshot = { status, nextAttemptAt: null };
  await tick();
}

describe('<TransportStatusBanner> first connection after a terminal state', () => {
  let reload: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    snapshot = { status: 'connected', nextAttemptAt: null };
    reload = vi.fn();
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { ...window.location, reload },
    });
  });

  it('boots again when a latched page finally connects', async () => {
    snapshot = { status: 'pairing-required', nextAttemptAt: null };
    render(TransportStatusBanner);
    await tick();
    expect(reload).not.toHaveBeenCalled();

    await publish('connected');
    expect(reload).toHaveBeenCalledOnce();
  });

  // The guard, and the reason it is worth having: a page that HAS been
  // connected holds loaded state and possibly text somebody typed, so a
  // session that dies mid-use and is signed back in keeps its page.
  it('keeps a page that had already connected once', async () => {
    render(TransportStatusBanner);
    await tick();

    await publish('unauthorized');
    await publish('connected');

    expect(reload).not.toHaveBeenCalled();
  });

  // An ordinary drop is not a terminal state — the ladder is still
  // running and the stores suspend rather than fail — so recovering from
  // one must not throw the page away either.
  it('keeps a page that only ever reconnected', async () => {
    snapshot = { status: 'reconnecting', nextAttemptAt: null };
    render(TransportStatusBanner);
    await tick();

    await publish('connected');
    expect(reload).not.toHaveBeenCalled();
  });
});
