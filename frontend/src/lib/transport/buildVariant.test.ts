import { afterEach, describe, expect, it } from 'vitest';
import { admitPairingLink, remoteAccessAvailable, __resetBuildVariantForTest } from './buildVariant';
import { stageNoRemoteBuild } from '../../test/helpers/buildVariant';

function stageMeta(content: string): void {
  const meta = document.createElement('meta');
  meta.name = 'ao-remote-access';
  meta.content = content;
  document.head.append(meta);
  __resetBuildVariantForTest();
}

describe('build variant', () => {
  it('is the standard build when the page carries no variant meta', () => {
    expect(remoteAccessAvailable()).toBe(true);
  });

  it('has no remote access when the meta says off', () => {
    stageNoRemoteBuild();
    expect(remoteAccessAvailable()).toBe(false);
  });

  it('treats any other content as the standard build', () => {
    stageMeta('on');
    expect(remoteAccessAvailable()).toBe(true);
  });

  it('reads the page once', () => {
    expect(remoteAccessAvailable()).toBe(true);
    const meta = document.createElement('meta');
    meta.name = 'ao-remote-access';
    meta.content = 'off';
    document.head.append(meta);
    expect(remoteAccessAvailable()).toBe(true);
    __resetBuildVariantForTest();
    expect(remoteAccessAvailable()).toBe(false);
  });
});

describe('pairing link at boot', () => {
  afterEach(() => history.replaceState(null, '', '/'));

  it('admits no link when there is none', () => {
    history.replaceState(null, '', '/?mode=local#elsewhere');
    expect(admitPairingLink()).toBe(false);
    expect(location.hash).toBe('#elsewhere');
  });

  it('admits a pairing link in the standard build and leaves it for the pairing screen', () => {
    history.replaceState(null, '', '/?mode=local#pair=secret');
    expect(admitPairingLink()).toBe(true);
    expect(location.hash).toBe('#pair=secret');
  });

  it('drops a pairing link and its secret in a build without remote access', () => {
    stageNoRemoteBuild();
    history.replaceState(null, '', '/?mode=local#pair=secret');
    expect(admitPairingLink()).toBe(false);
    expect(location.hash).toBe('');
    expect(location.search).toBe('?mode=local');
  });
});
