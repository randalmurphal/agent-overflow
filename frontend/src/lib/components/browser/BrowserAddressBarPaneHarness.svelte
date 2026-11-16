<script lang="ts">
  import type { ComponentProps } from 'svelte';
  import BrowserAddressBar from './BrowserAddressBar.svelte';
  import { provideBrowserPaneThread } from './browserPaneThread';

  // Test harness: the address row inside a browser pane of `threadId`, as
  // BrowserPane provides it, so the row's thread-scoped controls render.
  let { threadId, ...rest }: { threadId: string } & ComponentProps<typeof BrowserAddressBar> = $props();
  provideBrowserPaneThread(() => threadId);
</script>

<BrowserAddressBar {...rest} />
