// On-demand reader for a sent shell command's output. A timeline row carries
// only the first lines (the attachment's preview); the rest is read in pages
// when the person opens the block, and is held only while that block is
// mounted.
import { ReadShellAttachmentOutput } from '../stores/bindings';
import { errString } from './errors';

export interface ShellOutputReader {
  readonly expanded: boolean;
  readonly loading: boolean;
  readonly error: string;
  /** The output read so far, from the start of the file. */
  readonly data: string;
  /** The whole command, once a page has been read. */
  readonly command: string;
  readonly size: number;
  /** True when the file holds output past `data`. */
  readonly hasMore: boolean;
  /** True once any page has been read. */
  readonly loaded: boolean;
  open(): void;
  close(): void;
  loadMore(): Promise<void>;
}

export function createShellOutputReader(
  threadId: () => string,
  attachmentId: () => string,
): ShellOutputReader {
  let expanded = $state(false);
  let loading = $state(false);
  let error = $state('');
  let data = $state('');
  let command = $state('');
  let size = $state(0);
  let next = $state(0);
  let loaded = $state(false);
  // Identifies the read in flight, so an answer for a block that was closed
  // and reopened on other output, or loaded twice, is dropped.
  let generation = 0;

  async function loadMore(): Promise<void> {
    if (loading || (loaded && next >= size)) return;
    const mine = ++generation;
    const thread = threadId();
    const attachment = attachmentId();
    loading = true;
    error = '';
    try {
      const page = await ReadShellAttachmentOutput(thread, attachment, next, 0);
      if (mine !== generation) return;
      data += page.data ?? '';
      command = page.command ?? '';
      size = page.size ?? 0;
      next = page.next ?? size;
      loaded = true;
    } catch (err) {
      if (mine !== generation) return;
      error = errString(err);
    } finally {
      if (mine === generation) loading = false;
    }
  }

  return {
    get expanded() { return expanded; },
    get loading() { return loading; },
    get error() { return error; },
    get data() { return data; },
    get command() { return command; },
    get size() { return size; },
    get hasMore() { return loaded && next < size; },
    get loaded() { return loaded; },
    open() {
      expanded = true;
      if (!loaded) void loadMore();
    },
    close() {
      expanded = false;
    },
    loadMore,
  };
}
