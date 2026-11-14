// On-demand reader for a sent shell command's output. A timeline row carries
// only the first lines (the attachment's preview); the rest is read in pages
// when the person opens the block, and is held only while that block is
// mounted. Whether the block is open belongs to the pane's row state.
import { ReadShellAttachmentOutput } from '../stores/bindings';
import { errString } from './errors';

export interface ShellOutputReader {
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
  /** Reads the next page. One read runs at a time. */
  loadMore(): Promise<void>;
}

export function createShellOutputReader(
  threadId: () => string,
  attachmentId: () => string,
): ShellOutputReader {
  let loading = $state(false);
  let error = $state('');
  let data = $state('');
  let command = $state('');
  let size = $state(0);
  let next = $state(0);
  let loaded = $state(false);

  async function loadMore(): Promise<void> {
    if (loading || (loaded && next >= size)) return;
    loading = true;
    error = '';
    try {
      const page = await ReadShellAttachmentOutput(threadId(), attachmentId(), next, 0);
      data += page.data ?? '';
      command = page.command ?? '';
      size = page.size ?? 0;
      next = page.next ?? size;
      loaded = true;
    } catch (err) {
      error = errString(err);
    } finally {
      loading = false;
    }
  }

  return {
    get loading() { return loading; },
    get error() { return error; },
    get data() { return data; },
    get command() { return command; },
    get size() { return size; },
    get hasMore() { return loaded && next < size; },
    get loaded() { return loaded; },
    loadMore,
  };
}
