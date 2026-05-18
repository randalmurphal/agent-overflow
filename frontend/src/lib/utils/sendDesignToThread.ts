import {
  CreateThread,
  DeleteThread,
  GetDesignWorkdirInfo,
  SaveDraft,
  UploadAttachment,
} from '../stores/bindings';
import { prependThread } from '../stores/threads.svelte';
import { expandProject } from '../stores/sidebar.svelte';
import { addToast } from '../stores/toast.svelte';
import type { Thread } from '../types/models';
import type { PanelContext } from '../stores/rhsPanelSlot.svelte';
import { requestIframeCapture } from './captureHtml';
import { errString } from './errors';

// Cap on the number of file names listed in the seeded draft body.
// `ListMainFiles` is unbounded today, and an agent that stuffs main/
// would otherwise produce a multi-line draft body shipped over WS and
// persisted in SQLite. The cap is generous — typical designs have
// fewer than 10 files at top level. When truncated, the body says how
// many entries were elided so the user knows to ls the path themselves.
const MAX_MANIFEST_ENTRIES = 50;

// Best-effort cap on a single capture attempt before we fall back to
// "no screenshot". The iframe-side capture has its own internal
// timeout in requestIframeCapture; this is a defense-in-depth ceiling
// for cases where the iframe's reply hangs in unusual ways.
const CAPTURE_BEST_EFFORT_TIMEOUT_MS = 8_000;

// Backticks delimit the markdown code span we wrap the path / file
// names in. Filenames are filesystem-derived but the filesystem
// permits backticks in names; an agent or earlier user input could
// inject one. Stripping them is defense in depth for the LLM
// prompt-injection surface and for clean rendering in the chat
// transcript. Replace rather than escape so the body stays readable.
function sanitizeForCodeSpan(s: string): string {
  return s.replace(/`/g, '');
}

// `screenshotAttached` controls the trailing "screenshot is attached"
// line. We only include it when the capture round-trip actually
// produced a PNG that's been uploaded — otherwise the body would
// claim an attachment the new thread doesn't have, which confuses
// both the human reader and the agent reading the seed message.
export function buildSendToThreadDraftBody(
  mainPath: string,
  files: ReadonlyArray<string>,
  screenshotAttached: boolean,
): string {
  const safePath = sanitizeForCodeSpan(mainPath);
  const visible = files.slice(0, MAX_MANIFEST_ENTRIES).map(sanitizeForCodeSpan);
  const truncated = files.length > MAX_MANIFEST_ENTRIES;
  const fileLines = visible.length > 0
    ? visible.map((f) => `  - ${f}`).join('\n')
    : '  (no files yet)';
  const tail = truncated
    ? `\n  … and ${files.length - MAX_MANIFEST_ENTRIES} more`
    : '';
  const lines = [
    'Design context from another thread:',
    '',
    `Files at: \`${safePath}\``,
    'Manifest:',
    fileLines + tail,
    '',
  ];
  if (screenshotAttached) {
    lines.push('A screenshot of the current state is attached.', '');
  }
  // Trailing blank line so the cursor lands at the bottom of the
  // pre-filled context for the user's question.
  lines.push('');
  return lines.join('\n');
}

export interface SendDesignToThreadInput {
  ctx: PanelContext;
  iframe: HTMLIFrameElement;
}

export interface SendDesignToThreadResult {
  ok: boolean;
}

// captureScreenshotBestEffort wraps the iframe capture in a try/catch
// so a capture failure doesn't sink the whole send-to-thread flow.
// Capture is genuinely flaky inside our `sandbox="allow-scripts"`
// (no-allow-same-origin) iframe — modern-screenshot needs to access an
// internal helper iframe's contentDocument for default-style detection,
// which the opaque-origin sandbox blocks. The text body (path +
// manifest) is the load-bearing context; the screenshot is a nice-to-
// have. Returning null lets the caller proceed without an attachment.
async function captureScreenshotBestEffort(
  iframe: HTMLIFrameElement,
): Promise<string | null> {
  try {
    const requestId = crypto.randomUUID();
    const captureP = requestIframeCapture(iframe, requestId);
    const timeoutP = new Promise<never>((_resolve, reject) => {
      setTimeout(
        () => reject(new Error(`capture exceeded ${CAPTURE_BEST_EFFORT_TIMEOUT_MS}ms`)),
        CAPTURE_BEST_EFFORT_TIMEOUT_MS,
      );
    });
    return await Promise.race([captureP, timeoutP]);
  } catch (err) {
    // Console-warn (not toast) so the failure is debuggable but not
    // user-visible as an error — the caller surfaces a non-blocking
    // warning in the success toast instead.
    console.warn('design send-to-thread: screenshot capture failed:', err);
    return null;
  }
}

// Hand the in-progress design off to a brand-new chat thread inside
// the same project as the source design thread. Steps, in order:
//
//   1. Capture the live iframe (best-effort; see
//      captureScreenshotBestEffort above for why a failure isn't
//      fatal).
//   2. Look up the design workdir's absolute main/ path + manifest.
//   3. CreateThread (mode=chat) inheriting provider/model/runtime/
//      workspace from the source — the new thread is a sibling, not
//      a fresh context.
//   4. UploadAttachment for the screenshot if capture succeeded.
//      Skipped when the capture step returned null.
//   5. SaveDraft seeds the new thread's composer with the path +
//      manifest body and the uploaded attachment id (or no
//      attachments). We don't auto-send: the user reviews and types
//      the actual question.
//   6. prepend the new thread to the sidebar, expand its project,
//      and switch the pane.
//
// Failure semantics:
//   - Capture failure: warn-toast, proceed without an attachment.
//   - GetDesignWorkdirInfo / CreateThread failure: toast and return.
//   - UploadAttachment / SaveDraft failure after CreateThread: the
//     orphan thread row + any attachment row are deleted via
//     DeleteThread (cascades). Mirrors the rollback pattern in
//     `proposedPlanImplementation.ts` so a partial-write doesn't leave
//     a half-built thread sitting in the sidebar.
  //   - Thread switch during the awaits: each await re-checks the
  //     captured `ctx.threadId` against the entry value; on mismatch
//     we abort before the next mutation. Roll back any thread we
//     already created.
export async function sendDesignToThread({
  ctx,
  iframe,
}: SendDesignToThreadInput): Promise<SendDesignToThreadResult> {
  const sourceThread = ctx.thread;
  const sourceThreadId = ctx.threadId;
  if (!sourceThread || !sourceThreadId) return { ok: false };
  if (!sourceThread.projectId) {
    addToast('error', 'Send to thread: source thread is missing a project');
    return { ok: false };
  }

  let createdThreadId: string | null = null;
  try {
    const pngBase64 = await captureScreenshotBestEffort(iframe);
    if (ctx.threadId !== sourceThreadId) return { ok: false };

    const info = (await GetDesignWorkdirInfo(sourceThreadId)) as {
      mainPath: string;
      files: string[];
    };
    if (ctx.threadId !== sourceThreadId) return { ok: false };

    const created = (await CreateThread({
      projectId: sourceThread.projectId,
      provider: sourceThread.provider,
      model: sourceThread.model,
      mode: 'chat',
      reasoningEffort: sourceThread.reasoningEffort ?? '',
      fastMode: sourceThread.fastMode ?? null,
      contextWindow: sourceThread.contextWindow ?? 0,
      runtimeMode: sourceThread.runtimeMode ?? '',
      title: sourceThread.title ? `${sourceThread.title} – follow-up` : 'Design follow-up',
      workspaceOverride: sourceThread.workspacePath,
      worktreePath: sourceThread.worktreePath ?? '',
      branch: sourceThread.branch ?? '',
    })) as Thread;
    createdThreadId = created.id;

    const attachmentIds: string[] = [];
    if (pngBase64) {
      const attachment = await UploadAttachment(
        created.id,
        'design-preview.png',
        'image/png',
        pngBase64,
      );
      attachmentIds.push(attachment.id);
    }

    const body = buildSendToThreadDraftBody(
      info.mainPath,
      info.files,
      attachmentIds.length > 0,
    );
    await SaveDraft(created.id, body, attachmentIds, [], null);

    prependThread(created);
    if (created.projectId) expandProject(created.projectId);
    await ctx.switchThread(created);
    addToast(
      'success',
      pngBase64
        ? 'Opened new chat thread with design context'
        : 'Opened new chat thread (screenshot capture failed; path + files included)',
    );
    return { ok: true };
  } catch (err) {
    if (createdThreadId) {
      // Roll back the orphan thread row so it doesn't appear in the
      // sidebar after a failed seed. DeleteThread cascades to
      // attachments + drafts, so the upload (if it landed) goes too.
      await DeleteThread(createdThreadId).catch((cleanupErr) => {
        console.error('Failed to clean up orphan send-to-thread:', cleanupErr);
      });
    }
    addToast('error', `Send to thread failed: ${errString(err)}`);
    return { ok: false };
  }
}
