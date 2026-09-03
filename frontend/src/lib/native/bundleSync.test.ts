// The bundle-sync decision table, one row at a time.
//
// Everything this seam decides is in two pure functions — which backend
// to take a bundle from, and what to do about the one it offers — and
// this file is why they are pure. The alternative is proving an update
// mechanic through a plugin that only exists inside an APK, on a device
// this suite has never had.

import { describe, expect, it } from 'vitest';
import {
  MAX_ATTEMPTS_PER_BUNDLE,
  candidateFrom,
  decideBundleSync,
  pickBundleSource,
  type BundleCandidate,
  type BundleSyncInput,
} from './bundleSync';
import { HOME_BACKEND } from '../transport/backendKey';
import type { BundleState } from './plugins';
import type { TransportHello } from '../transport/wsClient';

const RUNNING = 'a'.repeat(64);
const OFFERED = 'b'.repeat(64);

function state(overrides: Partial<BundleState> = {}): BundleState {
  return {
    current: '',
    next: '',
    pendingHealth: '',
    lastKnownGood: '',
    rolledBack: [],
    versionCode: 7,
    ...overrides,
  };
}

function target(overrides: Partial<BundleCandidate> = {}): BundleCandidate {
  return {
    backend: HOME_BACKEND,
    backendName: 'desk',
    bundleId: OFFERED,
    bundleVersion: '1.2.3',
    minShellBuild: 1,
    ...overrides,
  };
}

function input(overrides: Partial<BundleSyncInput> = {}): BundleSyncInput {
  return {
    target: target(),
    running: RUNNING,
    state: state(),
    lease: 'active',
    inFlight: '',
    attempts: 0,
    ...overrides,
  };
}

describe('decideBundleSync', () => {
  it('downloads a bundle this phone does not have', () => {
    expect(decideBundleSync(input())).toEqual({
      kind: 'download',
      id: OFFERED,
      backend: HOME_BACKEND,
    });
  });

  it('does nothing when no attached backend serves a bundle', () => {
    // A dev-server boot, or a backend too old for the routes. Neither is
    // an error and neither is worth a log line.
    expect(decideBundleSync(input({ target: null }))).toEqual({ kind: 'idle' });
    expect(decideBundleSync(input({ target: target({ bundleId: '' }) }))).toEqual({ kind: 'idle' });
  });

  it('does nothing when the offered bundle is the one already running', () => {
    expect(decideBundleSync(input({ running: OFFERED }))).toEqual({ kind: 'idle' });
  });

  it('does nothing when the offered bundle is already staged for the next start', () => {
    // The window between staging and a restart is minutes or days. A
    // phone that re-downloaded across it would download the same bundle
    // on every reconnect for as long as nobody killed the app.
    expect(decideBundleSync(input({ state: state({ next: OFFERED }) }))).toEqual({ kind: 'idle' });
  });

  it('refuses a bundle that already failed its first boot here', () => {
    expect(decideBundleSync(input({ state: state({ rolledBack: [OFFERED] }) }))).toEqual({
      kind: 'rolled-back',
      id: OFFERED,
    });
  });

  it('declines a bundle whose floor is above this APK, and names the machine', () => {
    const decision = decideBundleSync(
      input({ target: target({ minShellBuild: 9, backendName: 'desk' }) }),
    );
    expect(decision).toEqual({ kind: 'too-old', id: OFFERED, backendName: 'desk' });
  });

  it('takes a bundle whose floor this APK exactly meets', () => {
    const decision = decideBundleSync({
      ...input({ target: target({ minShellBuild: 7 }) }),
      state: state({ versionCode: 7 }),
    });
    expect(decision.kind).toBe('download');
  });

  it('declines everything when the platform could not say what this APK is', () => {
    // versionCode 0 is below every floor a bundle can state, which is
    // the safe direction: a phone that cannot identify itself does not
    // get a bundle it might not be able to run.
    const decision = decideBundleSync({
      ...input({ target: target({ minShellBuild: 1 }) }),
      state: state({ versionCode: 0 }),
    });
    expect(decision.kind).toBe('too-old');
  });

  it('refuses a rolled-back bundle before it considers the floor', () => {
    const decision = decideBundleSync({
      ...input({ target: target({ minShellBuild: 99 }) }),
      state: state({ rolledBack: [OFFERED] }),
    });
    expect(decision.kind).toBe('rolled-back');
  });

  it('defers while the OS has the app paused', () => {
    expect(decideBundleSync(input({ lease: 'background' }))).toEqual({
      kind: 'deferred',
      id: OFFERED,
    });
  });

  it('joins a download of the same id rather than starting a second', () => {
    expect(decideBundleSync(input({ inFlight: OFFERED }))).toEqual({ kind: 'joined', id: OFFERED });
  });

  it('waits out a download of a different id rather than superseding it mid-transfer', () => {
    const other = 'c'.repeat(64);
    expect(decideBundleSync(input({ inFlight: other }))).toEqual({ kind: 'busy', id: OFFERED });
  });

  it('keeps downloading while attempts are left', () => {
    const decision = decideBundleSync(input({ attempts: MAX_ATTEMPTS_PER_BUNDLE - 1 }));
    expect(decision.kind).toBe('download');
  });

  it('stops fetching a bundle that has failed its cap of times', () => {
    // The cap has to be answerable HERE, because a failed attempt has to
    // answer the very next hello as well as its own retry timer. When the
    // cap lived only beside that timer, the decision kept saying
    // `download` and the archive was refetched with no delay and no end.
    expect(decideBundleSync(input({ attempts: MAX_ATTEMPTS_PER_BUNDLE }))).toEqual({
      kind: 'exhausted',
      id: OFFERED,
    });
  });

  it('joins a live download of the exhausted id rather than abandoning it', () => {
    // The cap is about starting a fetch, not about a transfer already
    // running: the one in flight may be the attempt that succeeds.
    const decision = decideBundleSync(
      input({ attempts: MAX_ATTEMPTS_PER_BUNDLE, inFlight: OFFERED }),
    );
    expect(decision).toEqual({ kind: 'joined', id: OFFERED });
  });

  it('answers the floor before the lease, so a phone that can never take it is told', () => {
    const decision = decideBundleSync(
      input({ target: target({ minShellBuild: 9 }), lease: 'background' }),
    );
    expect(decision.kind).toBe('too-old');
  });
});

describe('pickBundleSource', () => {
  const home = target({ backend: HOME_BACKEND, backendName: 'desk', bundleVersion: '1.2.3' });
  const other = target({
    backend: 'b-laptop',
    backendName: 'laptop',
    bundleId: 'd'.repeat(64),
    bundleVersion: '1.3.0',
  });

  it('answers nothing when nothing is attached', () => {
    expect(pickBundleSource([])).toBeNull();
  });

  it('ignores a backend that publishes no bundle', () => {
    expect(pickBundleSource([target({ bundleId: '' })])).toBeNull();
  });

  it('takes the newest version among attached backends, home or not', () => {
    expect(pickBundleSource([home, other])?.backend).toBe('b-laptop');
    expect(pickBundleSource([other, home])?.backend).toBe('b-laptop');
  });

  it('compares numerically rather than lexically', () => {
    const ten = target({ backend: 'b-ten', bundleVersion: '0.10.0' });
    const nine = target({ backend: 'b-nine', bundleVersion: '0.9.0' });
    expect(pickBundleSource([nine, ten])?.backend).toBe('b-ten');
  });

  it('tolerates a leading v and a pre-release suffix', () => {
    const tagged = target({ backend: 'b-tag', bundleVersion: 'v1.4.0-rc.1' });
    expect(pickBundleSource([home, tagged])?.backend).toBe('b-tag');
  });

  it('ranks a version it cannot parse below every version it can', () => {
    const dev = target({ backend: 'b-dev', bundleVersion: 'dev' });
    expect(pickBundleSource([dev, home])?.backend).toBe(HOME_BACKEND);
    expect(pickBundleSource([home, dev])?.backend).toBe(HOME_BACKEND);
  });

  it('prefers home on a tie', () => {
    const tie = target({ backend: 'b-laptop', bundleVersion: '1.2.3' });
    expect(pickBundleSource([tie, home])?.backend).toBe(HOME_BACKEND);
    expect(pickBundleSource([home, tie])?.backend).toBe(HOME_BACKEND);
  });

  it('prefers home when nothing parses at all, which is a fleet of dev builds', () => {
    const devHome = target({ backend: HOME_BACKEND, bundleVersion: 'dev' });
    const devOther = target({ backend: 'b-laptop', bundleVersion: 'dev' });
    expect(pickBundleSource([devOther, devHome])?.backend).toBe(HOME_BACKEND);
  });

  it('still answers when only unparseable versions are attached', () => {
    const devOther = target({ backend: 'b-laptop', bundleVersion: 'dev' });
    expect(pickBundleSource([devOther])?.backend).toBe('b-laptop');
  });
});

describe('candidateFrom', () => {
  function hello(overrides: Partial<TransportHello> = {}): TransportHello {
    return {
      protocolVersion: 1,
      capabilities: [],
      backendId: 'uuid',
      backendName: 'desk',
      serverTimeMs: 0,
      clockSkewMs: 0,
      bundleId: OFFERED,
      bundleVersion: '1.2.3',
      minShellBuild: 2,
      ...overrides,
    };
  }

  it('flattens a hello that carries a bundle', () => {
    expect(candidateFrom(HOME_BACKEND, hello())).toEqual({
      backend: HOME_BACKEND,
      backendName: 'desk',
      bundleId: OFFERED,
      bundleVersion: '1.2.3',
      minShellBuild: 2,
    });
  });

  it('answers nothing for a backend with no hello, or one that serves no bundle', () => {
    expect(candidateFrom(HOME_BACKEND, null)).toBeNull();
    expect(candidateFrom(HOME_BACKEND, hello({ bundleId: '' }))).toBeNull();
  });
});
