import { SvelteMap } from 'svelte/reactivity';
import {
  diffFilesReadSizes,
  diffFilesUnavailable,
  diffImageBlobKey,
  diffReadFailure,
  DIFF_FILE_SIZES_BATCH,
  fetchDiffImageBytes,
  readDiffFileSizes,
  readDiffImage,
  type DiffFilesTarget,
  type DiffReadAction,
  type DiffFileSizes,
  type DiffImageAnswer,
  type DiffImageSide,
} from './reviewDiffFiles';
import { acquireMediaBlob, objectOrDataUrl, type MediaBytes, type MediaHandle } from '../utils/mediaBlobCache';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { ReviewFile } from '../utils/patchStore';

// A review pane's binary and image files (docs/specs/pull-requests.md,
// "Image diffs"), for the diff on screen. A file row asks for its image
// while it is mounted (the list mounts only rows near the viewport) and
// releases it when it unmounts; the bytes are the media blob cache's
// (utils/mediaBlobCache.ts), which counts them against its budget, keeps
// them while shown and evicts them after. What an answer said (sizes,
// over the cap, not an image) is kept for the diff on screen, so a row
// that scrolls back paints its captions at once and reads only the
// bytes. Everything belongs to one diff: a new one (`setTarget` with a
// new scope key) drops it, and an answer for an older one is dropped.

export type ImageSideName = 'old' | 'new';

/** One side of an image row as it shows. */
export interface ImageSideView {
  side: ImageSideName;
  size: number;
  overCap: boolean;
  /** The bytes' URL once loaded; null while loading or over the cap. */
  url: string | null;
}

export type ImageRowState =
  | { kind: 'loading'; sides: readonly ImageSideName[] }
  | { kind: 'image'; sides: readonly ImageSideView[]; maxBytes: number }
  /** The file's ordinary binary row: not an image, unavailable, or a
   * refused read (`reason`, offering `action`: Retry, or reloading the
   * diff); `sizes` from the answer, or null when the row reads them. */
  | { kind: 'binary'; reason: string | null; action: DiffReadAction | null; sizes: DiffFileSizes | null };

export interface ImageHandle {
  readonly state: ImageRowState;
  /** Reads again after a refusal that offers Retry: a fresh read, once
   * per press. */
  retry(): void;
  release(): void;
}

export type SizesState =
  | { kind: 'loading' }
  | { kind: 'ready'; sizes: DiffFileSizes }
  | { kind: 'failed'; error: string };

export interface ReviewDiffMedia {
  /** The diff on screen; a new `scopeKey` drops what the last one held. */
  setTarget(target: DiffFilesTarget | null, scopeKey: string): void;
  /** Asks for a binary file's sizes (batched); no-op where sizes are not
   * read (edits) or already asked. */
  requestSizes(file: ReviewFile): void;
  sizes(path: string): SizesState | null;
  /** Whether this diff can read sizes at all. */
  readonly readsSizes: boolean;
  /** The image row of `file`, loading its bytes until released. */
  image(file: ReviewFile, sides: readonly ImageSideName[]): ImageHandle;
  /** The pixel size a side's image decoded at, for its caption. */
  pixels(path: string, side: ImageSideName): { width: number; height: number } | null;
  rememberPixels(path: string, side: ImageSideName, width: number, height: number): void;
}

// What an answer says, without its single-use URLs.
type AnswerFacts = Omit<DiffImageAnswer, 'old' | 'new'> & { old: Omit<DiffImageSide, 'url'>; new: Omit<DiffImageSide, 'url'> };

function facts(answer: DiffImageAnswer): AnswerFacts {
  const { url: _old, ...old } = answer.old;
  const { url: _new, ...neu } = answer.new;
  return { old, new: neu, maxBytes: answer.maxBytes };
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function createReviewDiffMedia(): ReviewDiffMedia {
  let target: DiffFilesTarget | null = null;
  let scopeKey = '';
  let generation = 0;
  let readsSizes = $state(false);
  const sizesByPath = new SvelteMap<string, SizesState>();
  const factsByPath = new Map<string, AnswerFacts>();
  const pixelsByKey = new SvelteMap<string, { width: number; height: number }>();
  let pendingSizes: ReviewFile[] = [];
  let sizesQueued = false;

  function flushSizes(): void {
    sizesQueued = false;
    const asked = target;
    const mine = generation;
    const files = pendingSizes;
    pendingSizes = [];
    if (!asked) return;
    for (let at = 0; at < files.length; at += DIFF_FILE_SIZES_BATCH) {
      const batch = files.slice(at, at + DIFF_FILE_SIZES_BATCH);
      readDiffFileSizes(asked, batch).then(
        (answers) => {
          if (mine !== generation) return;
          for (const file of batch) {
            const sizes = answers.get(file.path);
            sizesByPath.set(file.path, sizes && !sizes.error ? { kind: 'ready', sizes } : { kind: 'failed', error: sizes?.error ?? 'The sizes were not read.' });
          }
        },
        (err: unknown) => {
          if (mine !== generation) return;
          for (const file of batch) sizesByPath.set(file.path, { kind: 'failed', error: message(err) });
        },
      );
    }
  }

  function image(file: ReviewFile, sides: readonly ImageSideName[]): ImageHandle {
    let state = $state<ImageRowState>({ kind: 'loading', sides });
    let released = false;
    let attempt = 0;
    let blobs: MediaHandle<MediaBytes>[] = [];
    const path = file.path;

    function releaseBlobs(): void {
      for (const blob of blobs) blob.release();
      blobs = [];
    }

    function live(mine: number, asked: number): boolean {
      return !released && mine === attempt && asked === generation;
    }

    // The binary row: with the answer's sizes when there was one, else
    // the row reads them (`sizes`).
    function asBinary(reason: string | null, action: DiffReadAction | null, known: AnswerFacts | null = null): ImageRowState {
      if (!known) media.requestSizes(file);
      const sizes = known ? { old: { exists: known.old.exists, size: known.old.size }, new: { exists: known.new.exists, size: known.new.size } } : null;
      return { kind: 'binary', reason, action, sizes };
    }

    function refused(err: unknown): ImageRowState {
      const failure = diffReadFailure(err);
      return asBinary(failure.reason, failure.action);
    }

    async function start(): Promise<void> {
      const mine = ++attempt;
      const asked = generation;
      const diff = target;
      releaseBlobs();
      if (!diff) return;
      // No handle (the open could not pin the diff's files): the slot
      // says why; nothing is read.
      const unpinned = diffFilesUnavailable(diff);
      if (unpinned) {
        state = { kind: 'binary', reason: unpinned, action: null, sizes: null };
        return;
      }
      // One read per mount serves both sides' first fetch.
      let fresh: Promise<DiffImageAnswer> | null = null;
      const answer = (): Promise<DiffImageAnswer> => (fresh ??= readDiffImage(diff, file));
      let known = factsByPath.get(path);
      if (!known) {
        try {
          known = facts(await answer());
        } catch (err) {
          if (!live(mine, asked)) return;
          state = refused(err);
          return;
        }
        if (!live(mine, asked)) return;
        factsByPath.set(path, known);
      }
      const shown = sides.filter((side) => known[side].exists);
      const unavailable = shown.map((side) => known[side].unavailable).find(Boolean);
      if (unavailable) {
        state = asBinary(unavailable, null, known);
        return;
      }
      if (shown.length === 0 || shown.some((side) => known[side].notImage)) {
        state = asBinary(null, null, known);
        return;
      }
      const views: ImageSideView[] = shown.map((side) => ({ side, size: known[side].size, overCap: known[side].overCap ?? false, url: null }));
      state = { kind: 'image', sides: views, maxBytes: known.maxBytes };
      for (const view of views) {
        if (view.overCap) continue;
        const handle = acquireMediaBlob(diffImageBlobKey(diff, scopeKey, file, view.side), async () => {
          // Each ticket is spent once: a fetch after its read reads again.
          const side = (await answer())[view.side];
          fresh = null;
          if (!side.url) throw new Error(side.unavailable ?? 'The image is not served.');
          const blob = await fetchDiffImageBytes(diff, side.url);
          const mimeType = side.mimeType ?? blob.type;
          const typed = new Blob([blob], { type: mimeType });
          return { url: await objectOrDataUrl(typed, mimeType), mimeType, blob: typed };
        });
        blobs.push(handle);
        const show = (bytes: MediaBytes): void => {
          if (!live(mine, asked) || state.kind !== 'image') return;
          state = { ...state, sides: state.sides.map((each) => (each.side === view.side ? { ...each, url: bytes.url } : each)) };
        };
        if (handle.settled) show(handle.settled);
        else {
          handle.value.then(show, (err: unknown) => {
            // The first side to fail settles the row; the other's is moot.
            if (!live(mine, asked) || state.kind !== 'image') return;
            reportFrontendDiagnostic('review diff: an image could not be loaded', message(err));
            releaseBlobs();
            // Its facts may be stale (the diff changed since): read again
            // on Retry.
            factsByPath.delete(path);
            state = refused(err);
          });
        }
      }
    }

    void start();
    return {
      get state() { return state; },
      retry() {
        if (released || state.kind !== 'binary' || state.action !== 'retry') return;
        factsByPath.delete(path);
        state = { kind: 'loading', sides };
        void start();
      },
      release() {
        if (released) return;
        released = true;
        releaseBlobs();
      },
    };
  }

  const media: ReviewDiffMedia = {
    setTarget(next, key) {
      if (key === scopeKey && next !== null && target !== null) {
        if (target.kind === 'git' && target !== next) target.files.close();
        target = next;
        return;
      }
      generation += 1;
      // The diff on screen before is gone: its handle reopens no more.
      if (target?.kind === 'git' && target !== next) target.files.close();
      target = next;
      scopeKey = key;
      readsSizes = next !== null && diffFilesReadSizes(next);
      sizesByPath.clear();
      factsByPath.clear();
      pixelsByKey.clear();
      pendingSizes = [];
    },
    requestSizes(file) {
      if (!target || !diffFilesReadSizes(target) || sizesByPath.has(file.path) || diffFilesUnavailable(target)) return;
      sizesByPath.set(file.path, { kind: 'loading' });
      pendingSizes.push(file);
      if (sizesQueued) return;
      sizesQueued = true;
      queueMicrotask(flushSizes);
    },
    sizes: (path) => sizesByPath.get(path) ?? null,
    get readsSizes() { return readsSizes; },
    image,
    pixels: (path, side) => pixelsByKey.get(`${side}\u0000${path}`) ?? null,
    rememberPixels(path, side, width, height) {
      const key = `${side}\u0000${path}`;
      const known = pixelsByKey.get(key);
      if (width > 0 && height > 0 && (known?.width !== width || known?.height !== height)) pixelsByKey.set(key, { width, height });
    },
  };
  return media;
}

/** What the diff list's rows take from the pane for media rows: the
 * media, and the text SVGs shown rendered (the file header's toggle). */
export interface ReviewMediaHost {
  readonly media: ReviewDiffMedia;
  readonly newSideOnly: boolean;
  readonly renderedPaths: ReadonlySet<string>;
  toggleRendered(path: string): void;
  /** Reads the diff again: what a row offers when its file changed since
   * the diff was read. */
  reloadDiff(): void;
}
