import { describe, expect, it } from 'vitest';
import { installIOSInputZoomGuard, isIOSWebKit } from './iosInputZoom';

function nav(overrides: Partial<Navigator>): Navigator {
  return { userAgent: '', platform: '', maxTouchPoints: 0, ...overrides } as Navigator;
}

function docWithViewport(content: string | null): Document {
  const doc = document.implementation.createHTMLDocument('');
  if (content !== null) {
    const meta = doc.createElement('meta');
    meta.setAttribute('name', 'viewport');
    meta.setAttribute('content', content);
    doc.head.appendChild(meta);
  }
  return doc;
}

const VIEWPORT = 'width=device-width, initial-scale=1.0, interactive-widget=resizes-content';
const IPHONE = nav({ userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15', platform: 'iPhone' });
const IPAD_DESKTOP_UA = nav({ userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15', platform: 'MacIntel', maxTouchPoints: 5 });
const MAC = nav({ userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15', platform: 'MacIntel', maxTouchPoints: 0 });
const ANDROID = nav({ userAgent: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/128.0', platform: 'Linux armv8l', maxTouchPoints: 5 });

describe('isIOSWebKit', () => {
  it('recognizes an iPhone, and an iPad asking for desktop pages', () => {
    expect(isIOSWebKit(IPHONE)).toBe(true);
    expect(isIOSWebKit(IPAD_DESKTOP_UA)).toBe(true);
  });

  it('leaves a Mac and an Android phone alone', () => {
    expect(isIOSWebKit(MAC)).toBe(false);
    expect(isIOSWebKit(ANDROID)).toBe(false);
  });
});

describe('installIOSInputZoomGuard', () => {
  it('appends maximum-scale=1 to the viewport on iOS, once', () => {
    const doc = docWithViewport(VIEWPORT);
    installIOSInputZoomGuard(doc, IPHONE);
    installIOSInputZoomGuard(doc, IPHONE);
    expect(doc.querySelector('meta[name="viewport"]')!.getAttribute('content')).toBe(`${VIEWPORT}, maximum-scale=1`);
  });

  it('never touches the viewport off iOS: Android Chrome would lock pinch-zoom', () => {
    const doc = docWithViewport(VIEWPORT);
    installIOSInputZoomGuard(doc, ANDROID);
    installIOSInputZoomGuard(doc, MAC);
    expect(doc.querySelector('meta[name="viewport"]')!.getAttribute('content')).toBe(VIEWPORT);
  });

  it('is a no-op without a viewport meta', () => {
    const doc = docWithViewport(null);
    expect(() => installIOSInputZoomGuard(doc, IPHONE)).not.toThrow();
  });
});
