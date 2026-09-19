// How activating a forge attachment behaves on THIS page for the computer
// that owns the PR. One decision, shared by the click path
// (`forgeAttachmentActions.ts`) and the link title the parser writes
// (`forgeAttachmentExtension.ts`), so what the tooltip promises is what
// the click does.
//
//   save-here        The page runs on that computer's desktop: the bytes
//                    land in its Downloads folder, where the user is
//                    sitting.
//   open-externally  An embedded webview cannot service a browser
//                    download: neither the Wails webview nor the Android
//                    WebView handles `<a download>` on a blob URL, so the
//                    click would do nothing. That is the phone shell, and
//                    the desktop shell showing another computer's PR. The
//                    system browser is already signed into the forge, so
//                    it opens the forge URL there.
//   save-there       The same webview with no derivable forge URL. Writing
//                    the file on the owning computer and saying where is
//                    the one visible outcome left.
//   download         A connected browser: an ordinary browser download of
//                    the bytes the backend already fetched.

import type { BackendKey } from '../transport/backendKey';
import { hasScope } from '../transport/scopes';
import { isWebviewHosted } from '../transport/pageHost';
import { isNativeShell } from '../native/platform';

export type ForgeAttachmentAction = 'save-here' | 'open-externally' | 'save-there' | 'download';

export function forgeAttachmentAction(
  backend: BackendKey,
  browserUrl: string | null,
): ForgeAttachmentAction {
  // The shell is decided before the grant: a paired device is never on
  // the host whatever its session holds (`transport/scopes.ts` resolve),
  // and a file written on a distant computer is not what a phone asked for.
  const shell = isNativeShell();
  if (!shell && hasScope('host', backend)) return 'save-here';
  if (shell || isWebviewHosted()) return browserUrl ? 'open-externally' : 'save-there';
  return 'download';
}
