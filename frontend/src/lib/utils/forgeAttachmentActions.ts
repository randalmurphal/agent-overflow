// What activating a forge attachment does, in one place, because the anchor
// click delegate and the inline file chip must not drift. Which of the four
// outcomes applies is `forgeAttachmentAction`'s decision; this module only
// carries each one out and makes its result visible.

import { SaveForgeAttachment } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { attachedBackendEntry, backendDisplayName } from '../stores/attachedBackends.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { handleExternalURL } from './externalLinks';
import { errString } from './errors';
import { prReferenceWire } from './prReference';
import { forgeAttachmentAction } from './forgeAttachmentAction';
import { acquireForgeAttachment } from './forgeAttachmentCache';
import {
  browserUrlForForgeAttachment,
  forgeAttachmentName,
  type ParsedForgeAttachmentHref,
} from './forgeAttachments';

export async function openForgeAttachment(parsed: ParsedForgeAttachmentHref): Promise<void> {
  const browserUrl = browserUrlForForgeAttachment(
    parsed.pr.forge,
    parsed.href,
    parsed.webBase,
    parsed.pr,
  );
  const action = forgeAttachmentAction(parsed.backend, browserUrl);
  if (action === 'open-externally' && browserUrl) {
    await handleExternalURL(browserUrl);
    return;
  }
  if (action === 'save-here') {
    await saveOnOwningComputer(parsed, (path) => `Saved to ${path}`);
    return;
  }
  if (action === 'save-there') {
    // The path is on another computer, so the toast names it: a bare path
    // would read as a file on this one that is not there.
    await saveOnOwningComputer(
      parsed,
      (path) => `Saved on ${computerName(parsed.backend)}: ${path}`,
    );
    return;
  }
  await downloadForgeAttachment(parsed);
}

async function saveOnOwningComputer(
  parsed: ParsedForgeAttachmentHref,
  describe: (path: string) => string,
): Promise<void> {
  try {
    const path = await withBackendTarget(parsed.backend, () =>
      SaveForgeAttachment(prReferenceWire(parsed.pr), parsed.href),
    );
    addToast('success', describe(path));
  } catch (err) {
    addToast('error', errString(err));
  }
}

function computerName(backend: BackendKey): string {
  const entry = attachedBackendEntry(backend);
  return entry ? backendDisplayName(entry) : backend;
}

async function downloadForgeAttachment(parsed: ParsedForgeAttachmentHref): Promise<void> {
  const handle = acquireForgeAttachment(parsed.backend, parsed.pr, parsed.href);
  try {
    const resolved = await handle.value;
    if (typeof document === 'undefined') return;
    const anchor = document.createElement('a');
    anchor.href = resolved.url;
    anchor.download = resolved.filename || forgeAttachmentName(parsed.pr.forge, parsed.href);
    anchor.rel = 'noopener';
    anchor.style.display = 'none';
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
  } catch (err) {
    addToast('error', errString(err));
  } finally {
    // The click has already handed the URL to the download by the time it
    // returns, so this holder's retention can go.
    handle.release();
  }
}
