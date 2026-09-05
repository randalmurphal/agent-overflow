/**
 * Plain-text clipboard write. Resolves `false` when the clipboard refused
 * or is absent (no API outside a secure context — reachable from a remote
 * client served over plain HTTP); every caller turns that into a visible
 * "copy failed" state. The boolean is all a caller can act on, but the
 * rejection itself still gets logged and persisted: which DOMException it was is the
 * only thing that distinguishes a denied permission from an unfocused
 * document, and the caller can't carry it.
 */
let diagnosticsSink: ((message: string, detail: string) => void) | null = null;
// Installed by frontendErrorCapture so this leaf utility (and CopyButton)
// does not import the app's stores or bindings.
export function setClipboardDiagnosticsSink(sink: typeof diagnosticsSink): void {
  diagnosticsSink = sink;
}

export function reportCopyFailure(message: string, error: unknown, detail = ''): void {
  console.error(message, error);
  diagnosticsSink?.(message, `${detail}\n${String(error)}\n${error instanceof Error ? error.stack ?? '' : ''}`);
}

export async function copyToClipboard(text: string, event?: MouseEvent): Promise<boolean> {
  // Capture before the write: activation/focus may be lost while it rejects.
  const focused = document.hasFocus();
  const active = navigator.userActivation?.isActive;
  const writeAt = performance.now();
  const eventAgeAtWrite = event ? writeAt - event.timeStamp : undefined;
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (err) {
    // Never include the payload. Production disables the inspector, so the
    // caught rejection must reach the ordinary, bounded diagnostic log.
    reportCopyFailure('Clipboard write failed:', err, JSON.stringify({
      secureContext: globalThis.isSecureContext,
      clipboardAvailable: typeof navigator.clipboard?.writeText === 'function',
      focusedAtWrite: focused,
      activeAtWrite: active,
      eventTrusted: event?.isTrusted,
      eventType: event?.type,
      eventDetail: event?.detail,
      eventAgeAtWrite,
      focusedAtFailure: document.hasFocus(),
      activeAtFailure: navigator.userActivation?.isActive,
    }));
    return false;
  }
}
