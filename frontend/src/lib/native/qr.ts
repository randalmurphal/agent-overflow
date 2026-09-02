// The camera, for exactly one job: reading a pairing link off the QR code
// on the owner's own screen (Settings, Devices).
//
// What comes back is TEXT and this module makes nothing of it. The
// scanned string is a pairing URL whose `#pair=` fragment
// `transport/deviceSession.parsePairingFragment` already understands, and
// parsing it here would be a second reader of a format that has one — the
// same argument `passkey.ts` makes about its decode being keyed on the
// specification's names rather than on what a value looks like.

import { qrCodeHint, scannerPlugin } from './plugins';
import { isNativeShell } from './platform';

/**
 * Scan one QR code. Null when there is no camera to ask (every browser
 * build), and null when the person backed out of the scanner — which is
 * not an error and must not be reported as one.
 */
export async function scanPairingQr(): Promise<string | null> {
  if (!isNativeShell()) return null;
  const scanner = await scannerPlugin();
  if (!scanner) return null;
  try {
    const result = await scanner.scanBarcode({ hint: await qrCodeHint() });
    const text = result?.ScanResult ?? '';
    return text === '' ? null : text;
  } catch {
    // Cancelled, or permission declined. Both leave the caller exactly
    // where it was.
    return null;
  }
}
