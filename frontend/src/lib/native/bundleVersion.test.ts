import { describe, expect, it } from 'vitest';
import { compareBundleVersions } from './bundleVersion';

describe('bundle release precedence', () => {
  it('orders releases and prereleases without build metadata or numeric precision loss', () => {
    const versions = ['1.0.0-alpha', '1.0.0-alpha.1', '1.0.0-alpha.beta', '1.0.0-beta', '1.0.0-beta.2',
      '1.0.0-beta.11', '1.0.0-rc.1', '1.0.0', '1.0.1', '1.10.0', '2.0.0', '99999999999999999999.0.0'];
    for (let i = 0; i < versions.length; i++) {
      for (let j = 0; j < versions.length; j++) {
        expect(compareBundleVersions(versions[i], versions[j])).toBe(Math.sign(i - j));
      }
    }
    expect(compareBundleVersions('1.0.0+old', '1.0.0+new')).toBe(0);
  });

  it.each(['dev', '', '1.0', 'v1.0.0', '01.0.0', '1.0.0-01', '1.0.0-rc..1', '1.0.0 trailing', '1.0.0+'])('refuses unknown precedence for %s', (version) => {
    expect(compareBundleVersions(version, '1.0.0')).toBeNull();
    expect(compareBundleVersions('1.0.0', version)).toBeNull();
  });
});
