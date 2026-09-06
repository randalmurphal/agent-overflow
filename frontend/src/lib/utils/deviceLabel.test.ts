import { describe, expect, it } from 'vitest';
import { devicePlatform, suggestDeviceLabel } from './deviceLabel';

describe('devicePlatform', () => {
  it.each([
    ['Linux; Android 16; Pixel 9', 'Linux aarch64', 'Android'],
    ['iPhone; CPU iPhone OS 17_0 like Mac OS X', 'iPhone', 'iOS'],
    ['iPad; CPU OS 17_0 like Mac OS X', 'iPad', 'iPadOS'],
    ['Macintosh; Intel Mac OS X', 'MacIntel', 'macOS'],
    ['Windows NT 10.0; Win64', 'Win32', 'Windows'],
    ['X11; Linux aarch64', 'Linux aarch64', 'Linux'],
    ['Unknown', 'Other OS', 'Other OS'],
    ['Unknown', '', ''],
  ])('describes %s without mistaking the kernel for the OS', (ua, raw, expected) => {
    expect(devicePlatform(ua, raw)).toBe(expected);
  });
});

describe('suggestDeviceLabel', () => {
  it('names the platform the user agent describes', () => {
    expect(
      suggestDeviceLabel(
        'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15',
      ),
    ).toBe('iPhone');
    expect(
      suggestDeviceLabel('Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15'),
    ).toBe('iPad');
    expect(
      suggestDeviceLabel('Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36'),
    ).toBe('Android phone');
    expect(
      suggestDeviceLabel('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36'),
    ).toBe('Mac browser');
    expect(
      suggestDeviceLabel('Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36'),
    ).toBe('Windows browser');
    expect(suggestDeviceLabel('Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36')).toBe(
      'Linux browser',
    );
  });

  it('falls back to a generic label for anything else', () => {
    expect(suggestDeviceLabel('SomethingNobodyShips/1.0')).toBe('Browser');
  });

  it('reads iPad before the Mac token Safari also carries', () => {
    // iPadOS user agents contain "like Mac OS X"; the order of the
    // checks is what keeps an iPad from being named a Mac.
    expect(
      suggestDeviceLabel('Mozilla/5.0 (iPad; CPU OS 16_6 like Mac OS X) AppleWebKit/605.1.15'),
    ).toBe('iPad');
  });
});
