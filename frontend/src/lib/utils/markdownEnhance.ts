// Post-render delegates for ChatMarkdown.
//
// The legacy DOM-walker linkifier is gone. Path links are emitted as
// real markdown `link` tokens by `pathLinkExtension.ts` during the
// initial marked parse, so by the time the DOM exists each path is
// already a settled `<a href="agent-overflow:open?…">` anchor. The
// only remaining responsibilities here are document-level delegates:
//
//   - **Markdown-aware copy** — re-exported from
//     `markdownCopyDelegate.ts`. Installed once per page lifetime.
//   - **Path-link click** — routes clicks on `agent-overflow:open?…`
//     anchors to the `OpenInEditor` Go binding. Installed once per
//     page lifetime.
//
// Neither delegate cares which surface mounted the markdown; both
// match on attributes / classes that any rendered tree can carry.

import { openInEditor } from '../stores/openInEditor';
import { addToast } from '../stores/toast.svelte';
import { errString } from './errors';
import { PATH_LINK_HREF_PREFIX, parsePathLinkHref } from './pathLinkExtension';
import { hasScope } from '../transport/scopes';

export {
  ensureMarkdownCopyDelegate,
  __resetMarkdownCopyDelegateForTest,
} from './markdownCopyDelegate';

let pathLinkDelegateInstalled = false;

/**
 * Install the document-level click delegate that intercepts clicks on
 * path-link anchors and forwards them to the `OpenInEditor` binding.
 * Idempotent: subsequent calls are no-ops.
 */
export function ensurePathLinkClickDelegate(): void {
  if (pathLinkDelegateInstalled) return;
  if (typeof document === 'undefined') return;
  pathLinkDelegateInstalled = true;
  document.addEventListener('click', handlePathLinkClick);
  document.addEventListener('auxclick', suppressPathLinkAuxClick);
}

function handlePathLinkClick(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 0) return;
  const target = event.target;
  if (!(target instanceof Element)) return;
  const link = target.closest<HTMLAnchorElement>('a[href]');
  if (!link) return;
  const href = link.getAttribute('href');
  if (!href || !href.startsWith(PATH_LINK_HREF_PREFIX)) return;
  const parsed = parsePathLinkHref(href);
  if (!parsed) return;
  event.preventDefault();
  // The anchor may have been rendered before bootstrap established where
  // this page is. Keep the document-level boundary safe across that
  // transition even though ChatMarkdown stops emitting new path links.
  if (!hasScope('host')) return;
  void invokePathLink(parsed.path, parsed.line, parsed.col, parsed.workspacePath);
}

// Middle/right-button flows on a path-link anchor must not reach the
// browser: the href is an unregistered custom scheme on an
// `<a target="_blank">`, so a middle-click "open in new tab" becomes an
// external-protocol-handler request carrying the link's query if
// anything on the host ever claims `agent-overflow:`. Only the primary
// click (above) activates the editor open.
function suppressPathLinkAuxClick(event: MouseEvent): void {
  const target = event.target;
  if (!(target instanceof Element)) return;
  const link = target.closest<HTMLAnchorElement>('a[href]');
  if (!link) return;
  const href = link.getAttribute('href');
  if (!href || !href.startsWith(PATH_LINK_HREF_PREFIX)) return;
  event.preventDefault();
}

async function invokePathLink(
  path: string,
  line: number,
  col: number,
  workspacePath: string,
): Promise<void> {
  try {
    // Empty editorID → open in the user's default editor (preference →
    // catalog → $EDITOR). Path links never target a specific editor.
    await openInEditor(path, line, col, workspacePath, '');
  } catch (err) {
    addToast('error', errString(err));
  }
}

// Test-only — let specs assert install behavior across cases without
// leaking listeners across test files.
export function __resetPathLinkDelegateForTest(): void {
  if (pathLinkDelegateInstalled && typeof document !== 'undefined') {
    document.removeEventListener('click', handlePathLinkClick);
    document.removeEventListener('auxclick', suppressPathLinkAuxClick);
  }
  pathLinkDelegateInstalled = false;
}
