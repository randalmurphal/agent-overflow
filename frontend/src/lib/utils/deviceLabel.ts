// A readable default name for THIS device, guessed from the browser's
// own description of itself. Two surfaces name the same machine for the
// same owner-facing lists — the pairing screen naming a new device, and
// the passkey block naming a credential — so they share one guess
// rather than two that drift.
export function suggestDeviceLabel(ua: string = navigator.userAgent): string {
  if (/iPad/.test(ua)) return 'iPad';
  if (/iPhone/.test(ua)) return 'iPhone';
  if (/Android/.test(ua)) return 'Android phone';
  if (/Mac/.test(ua)) return 'Mac browser';
  if (/Windows/.test(ua)) return 'Windows browser';
  if (/Linux/.test(ua)) return 'Linux browser';
  return 'Browser';
}
