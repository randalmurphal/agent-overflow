// Mobile Safari zooms the page in when an input or textarea with text under
// 16px takes focus, and never zooms back out. Every iPhone browser is
// WebKit, so the rule reaches all of them, and the WKWebView an iOS shell
// would use. The app's base text is 13px by the owner's choice, so the
// zoom is turned off rather than the text turned up: since iOS 10 Safari
// ignores `maximum-scale` for pinch-zoom (an accessibility decision) but
// still honors it for the focus zoom. Only iOS gets the directive: Android
// Chrome honors it for pinch-zoom too and would lock the page.

export function isIOSWebKit(nav: Navigator = navigator): boolean {
  if (/iPad|iPhone|iPod/.test(nav.userAgent)) return true;
  // iPadOS asks for desktop pages and reports itself as a Mac; touch
  // points tell it apart from a real Mac.
  return nav.platform === 'MacIntel' && (nav.maxTouchPoints ?? 0) > 1;
}

const DIRECTIVE = 'maximum-scale=1';

/** Idempotent; call once before mount. */
export function installIOSInputZoomGuard(doc: Document = document, nav: Navigator = navigator): void {
  if (!isIOSWebKit(nav)) return;
  const meta = doc.querySelector<HTMLMetaElement>('meta[name="viewport"]');
  if (!meta) return;
  const content = meta.getAttribute('content') ?? '';
  if (content.includes('maximum-scale')) return;
  meta.setAttribute('content', content ? `${content}, ${DIRECTIVE}` : DIRECTIVE);
}
