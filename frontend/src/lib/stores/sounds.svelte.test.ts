import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import {
  __resetCustomSoundsForTest,
  addCustomSound,
  customSoundUrl,
  customSoundsError,
  customSoundsLoaded,
  deleteCustomSound,
  ensureCustomSounds,
  onCustomSoundsChanged,
  peekCustomSounds,
} from './sounds.svelte';

// The store's own subject is the OBJECT-URL LEDGER, so every test watches the
// two calls that make and unmake one. happy-dom mints real blob URLs, so the
// spies wrap the genuine implementations rather than replacing them.
let created: string[] = [];
let revoked: string[] = [];

beforeEach(() => {
  created = [];
  revoked = [];
  const mint = URL.createObjectURL.bind(URL);
  const drop = URL.revokeObjectURL.bind(URL);
  vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob | MediaSource) => {
    const url = mint(blob);
    created.push(url);
    return url;
  });
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation((url: string) => {
    revoked.push(url);
    drop(url);
  });
});

afterEach(() => {
  __resetCustomSoundsForTest();
  resetBindingMocks();
  vi.restoreAllMocks();
});

/** base64 of `bytes`, so a test can state the wire body it expects back. */
function wireWav(bytes: number[]): string {
  return btoa(String.fromCharCode(...bytes));
}

function seedListing(
  sounds: Array<{ id: string; wav: string }>,
  warnings: string[] = [],
  dir = '/cfg/sounds',
): void {
  setBindingMock('GetSoundFiles', () => ({ dir, sounds, warnings }));
}

async function load(): Promise<void> {
  ensureCustomSounds();
  await vi.waitFor(() => {
    expect(customSoundsLoaded()).toBe(true);
  });
}

describe('custom sounds store', () => {
  it('turns each cue into one object URL and carries the backend directory', async () => {
    seedListing(
      [
        { id: 'desk-bell', wav: wireWav([1, 2, 3]) },
        { id: 'chime', wav: wireWav([4, 5]) },
      ],
      ['bogus.wav: skipped, not a canonical cue'],
    );
    await load();

    const listing = peekCustomSounds();
    expect(listing.dir).toBe('/cfg/sounds');
    expect(listing.sounds.map((sound) => sound.id)).toEqual(['desk-bell', 'chime']);
    expect(listing.warnings).toEqual(['bogus.wav: skipped, not a canonical cue']);
    expect(created).toHaveLength(2);
    expect(new Set(listing.sounds.map((sound) => sound.url)).size).toBe(2);
    expect(revoked).toEqual([]);
  });

  // The player asks by id and must be told "no" rather than handed a URL that
  // plays nothing, because a missing cue has a defined substitution.
  it('answers customSoundUrl for a cue it holds and null for one it does not', async () => {
    seedListing([{ id: 'desk-bell', wav: wireWav([1, 2, 3]) }]);
    await load();

    expect(customSoundUrl('desk-bell')).toBe(peekCustomSounds().sounds[0].url);
    expect(customSoundUrl('never-added')).toBeNull();
  });

  // Nothing before the first listing arrives may claim a cue is absent: the
  // Settings warning and the player's substitution both turn on that answer.
  it('reports the library as unloaded until the first listing resolves', async () => {
    expect(customSoundsLoaded()).toBe(false);
    expect(peekCustomSounds().sounds).toEqual([]);
    seedListing([{ id: 'desk-bell', wav: wireWav([1]) }]);
    await load();
    expect(customSoundsLoaded()).toBe(true);
  });

  // The watcher's nudge is the only way an edit made outside this screen —
  // another browser, an agent writing the file — becomes visible here.
  it('re-lists on sound:changed, revoking exactly the superseded URLs', async () => {
    seedListing([{ id: 'desk-bell', wav: wireWav([1, 2, 3]) }]);
    await load();
    const stale = peekCustomSounds().sounds[0].url;

    const changed = vi.fn();
    const cancel = onCustomSoundsChanged(changed);
    seedListing([{ id: 'chime', wav: wireWav([9]) }]);
    emitWailsEvent('sound:changed', null);

    await vi.waitFor(() => {
      expect(peekCustomSounds().sounds.map((sound) => sound.id)).toEqual(['chime']);
    });
    expect(revoked).toEqual([stale]);
    expect(customSoundUrl('desk-bell')).toBeNull();
    expect(changed).toHaveBeenCalled();
    cancel();
  });

  // A screen that never plays or configures a cue is not subscribed at all,
  // so the watcher's nudge cannot start a fetch nobody asked for.
  it('ignores sound:changed while nothing holds the library', async () => {
    const list = setBindingMock('GetSoundFiles', () => ({ dir: '', sounds: [], warnings: [] }));
    emitWailsEvent('sound:changed', null);
    await Promise.resolve();
    expect(list).not.toHaveBeenCalled();
  });

  // The backend validated what it encoded, so a body that is not base64 is a
  // mangled frame. It costs one row and explains itself.
  it('keeps the rest of the library when one row does not decode', async () => {
    seedListing([
      { id: 'good', wav: wireWav([1, 2]) },
      { id: 'mangled', wav: '!!! not base64 !!!' },
    ]);
    await load();

    const listing = peekCustomSounds();
    expect(listing.sounds.map((sound) => sound.id)).toEqual(['good']);
    expect(listing.warnings).toEqual(['mangled: skipped, the sound did not arrive intact']);
  });

  it('sends an added cue as base64 and re-reads the listing the host now has', async () => {
    seedListing([]);
    await load();
    const put = setBindingMock('PutSoundFile', () => undefined);
    seedListing([{ id: 'desk-bell', wav: wireWav([7, 8, 9]) }]);

    await addCustomSound('desk-bell', new Uint8Array([7, 8, 9]));

    expect(put.mock.calls[0]).toEqual(['desk-bell', wireWav([7, 8, 9])]);
    await vi.waitFor(() => {
      expect(customSoundUrl('desk-bell')).not.toBeNull();
    });
  });

  // `String.fromCharCode(...bytes)` on a whole cue is 264644 arguments, which
  // overflows the call stack on every engine. The encoder chunks; this is the
  // test that would catch a rewrite that stopped.
  it('encodes a full-length cue without overflowing the call stack', async () => {
    seedListing([]);
    await load();
    const put = setBindingMock('PutSoundFile', () => undefined);
    const wav = new Uint8Array(264644);
    for (let i = 0; i < wav.length; i += 1) wav[i] = i % 256;

    await addCustomSound('long-one', wav);

    const body = put.mock.calls[0][1] as string;
    expect(atob(body)).toHaveLength(wav.length);
  });

  // A refused write must reach the caller. Settings shows the message; a
  // swallowed rejection would leave a button that does nothing.
  it('rejects an add the host refuses and leaves the listing alone', async () => {
    seedListing([]);
    await load();
    setBindingMock('PutSoundFile', () => {
      throw new Error('a sound named "desk-bell" already exists; delete it first');
    });

    await expect(addCustomSound('desk-bell', new Uint8Array([1]))).rejects.toThrow('already exists');
    expect(peekCustomSounds().sounds).toEqual([]);
  });

  it('deletes through the host and re-reads, and rejects a refusal', async () => {
    seedListing([{ id: 'desk-bell', wav: wireWav([1]) }]);
    await load();
    const remove = setBindingMock('DeleteSoundFile', () => undefined);
    seedListing([]);

    await deleteCustomSound('desk-bell');
    expect(remove.mock.calls[0]).toEqual(['desk-bell']);
    await vi.waitFor(() => {
      expect(peekCustomSounds().sounds).toEqual([]);
    });

    setBindingMock('DeleteSoundFile', () => {
      throw new Error('not a valid sound id');
    });
    await expect(deleteCustomSound('../etc/passwd')).rejects.toThrow('not a valid sound id');
  });

  // A backend that cannot answer is a visible state, not an empty library
  // that silently offers nothing.
  it('surfaces a failed listing as an error rather than an empty library', async () => {
    setBindingMock('GetSoundFiles', () => {
      throw new Error('sounds directory is unreadable');
    });
    ensureCustomSounds();

    await vi.waitFor(() => {
      expect(customSoundsError()).toContain('unreadable');
    });
    expect(peekCustomSounds().sounds).toEqual([]);
    expect(getBindingMock('GetSoundFiles')).toHaveBeenCalled();
  });

  // Releasing the last hold is the one moment nothing on screen references the
  // URLs, so it is the one moment they can be revoked.
  it('revokes every URL when the library is dropped', async () => {
    seedListing([
      { id: 'a', wav: wireWav([1]) },
      { id: 'b', wav: wireWav([2]) },
    ]);
    await load();
    const minted = peekCustomSounds().sounds.map((sound) => sound.url);

    __resetCustomSoundsForTest();

    expect(revoked.sort()).toEqual([...minted].sort());
    expect(customSoundUrl('a')).toBeNull();
  });

  // The player registers ONE module-scope listener and never registers again,
  // so a reset that dropped it would leave it holding revoked URLs forever.
  it('keeps listeners registered across a reset', async () => {
    const changed = vi.fn();
    const cancel = onCustomSoundsChanged(changed);
    seedListing([{ id: 'desk-bell', wav: wireWav([1]) }]);
    await load();
    const before = changed.mock.calls.length;

    __resetCustomSoundsForTest();
    seedListing([{ id: 'chime', wav: wireWav([2]) }]);
    await load();

    expect(changed.mock.calls.length).toBeGreaterThan(before);
    cancel();
  });
});
