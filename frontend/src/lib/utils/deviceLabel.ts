// A readable default name for THIS device, guessed from the browser's
// own description of itself. Pairing and passkey surfaces share this
// guess; a destination computer's name never describes this client.
export function suggestDeviceLabel(ua: string = navigator.userAgent): string {
  switch (devicePlatform(ua, '')) {
    case 'iPadOS': return 'iPad';
    case 'iOS': return 'iPhone';
    case 'Android': return 'Android phone';
    case 'macOS': return 'Mac browser';
    case 'Windows': return 'Windows browser';
    case 'Linux': return 'Linux browser';
    default: return 'Browser';
  }
}

/** Presentation only. Android's navigator.platform can report Linux aarch64. */
export function devicePlatform(ua: string = navigator.userAgent, fallback: string = navigator.platform || ''): string {
  if (/iPad/.test(ua)) return 'iPadOS';
  if (/iPhone/.test(ua)) return 'iOS';
  if (/Android/.test(ua)) return 'Android';
  if (/Mac/.test(ua)) return 'macOS';
  if (/Windows/.test(ua)) return 'Windows';
  if (/Linux/.test(ua)) return 'Linux';
  return fallback;
}
