<script lang="ts">
  import BrowserJarControl from './BrowserJarControl.svelte';
  import { provideBrowserPaneThread } from './browserPaneThread';

  // Test harness: the jar control inside a pane that provides `threadId`.
  let { threadId }: { threadId: string } = $props();
  provideBrowserPaneThread(() => threadId);
</script>

<BrowserJarControl />
