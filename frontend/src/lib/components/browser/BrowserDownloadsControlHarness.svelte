<script lang="ts">
  import BrowserDownloadsControl from './BrowserDownloadsControl.svelte';
  import { provideBrowserPaneThread } from './browserPaneThread';

  // Test harness: the downloads control inside a pane that provides `threadId`.
  let { threadId }: { threadId: string } = $props();
  provideBrowserPaneThread(() => threadId);
</script>

<BrowserDownloadsControl />
