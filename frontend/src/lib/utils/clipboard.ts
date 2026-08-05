/**
 * Plain-text clipboard write. Resolves `false` when the clipboard refused
 * or is absent (no API outside a secure context — reachable from a remote
 * client served over plain HTTP); every caller turns that into a visible
 * "copy failed" state. The boolean is all a caller can act on, but the
 * rejection itself still gets logged: which DOMException it was is the
 * only thing that distinguishes a denied permission from an unfocused
 * document, and the caller can't carry it.
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (err) {
    console.error('Clipboard write failed:', err);
    return false;
  }
}
