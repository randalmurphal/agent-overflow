// What saving a file the backend holds does on THIS page, for the computer
// that owns it. One decision, shared by forge attachments
// (`forgeAttachmentActions.ts`, and the link title `forgeAttachmentExtension.ts`
// writes, so what the tooltip promises is what the click does) and image
// attachments (`imageMenuActions.ts`).
//
//   save-here        The page runs on that computer's desktop: the bytes
//                    land in its Downloads folder, where the user is
//                    sitting.
//   open-externally  An embedded webview cannot service a browser
//                    download: neither the Wails webview nor the Android
//                    WebView handles `<a download>` on a blob URL, so the
//                    click would do nothing. That is the phone shell, and
//                    the desktop shell showing another computer's file.
//                    When the file has a URL the system browser can open
//                    (`browserUrl`), it opens there.
//   save-there       The same webview with no such URL. Writing the file on
//                    the owning computer and saying where is the one
//                    visible outcome left.
//   download         A connected browser: an ordinary browser download of
//                    the bytes.

import type { BackendKey } from '../transport/backendKey';
import { hasScope } from '../transport/scopes';
import { isWebviewHosted } from '../transport/pageHost';
import { isNativeShell } from '../native/platform';
import { attachedBackendEntry, backendDisplayName } from '../stores/attachedBackends.svelte';

export type FileSaveAction = 'save-here' | 'open-externally' | 'save-there' | 'download';

export function fileSaveAction(backend: BackendKey, browserUrl: string | null): FileSaveAction {
  // The shell is decided before the grant: a paired device is never on
  // the host whatever its session holds (`transport/scopes.ts` resolve),
  // and a file written on a distant computer is not what a phone asked for.
  const shell = isNativeShell();
  if (!shell && hasScope('host', backend)) return 'save-here';
  if (shell || isWebviewHosted()) return browserUrl ? 'open-externally' : 'save-there';
  return 'download';
}

/**
 * The toast for a file the owning computer wrote at `path`. A save there
 * names the computer: a bare path would read as a file on this one.
 */
export function savedFileMessage(
  action: 'save-here' | 'save-there',
  backend: BackendKey,
  path: string,
): string {
  if (action === 'save-here') return `Saved to ${path}`;
  const entry = attachedBackendEntry(backend);
  return `Saved on ${entry ? backendDisplayName(entry) : backend}: ${path}`;
}
